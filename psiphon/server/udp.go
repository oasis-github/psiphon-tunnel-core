/*
 * Copyright (c) 2016, Psiphon Inc.
 * All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon"
	"golang.org/x/crypto/ssh"
)

// setUDPChannel sets the single UDP channel for this sshClient.
// Each sshClient may have only one concurrent UDP channel. Each
// UDP channel multiplexes many UDP port forwards via the udpgw
// protocol. Any existing UDP channel is closed.
func (sshClient *sshClient) setUDPChannel(channel ssh.Channel) {
	sshClient.Lock()
	if sshClient.udpChannel != nil {
		sshClient.udpChannel.Close()
	}
	sshClient.udpChannel = channel
	sshClient.Unlock()
}

// handleUDPChannel implements UDP port forwarding. A single UDP
// SSH channel follows the udpgw protocol, which multiplexes many
// UDP port forwards.
//
// The udpgw protocol and original server implementation:
// Copyright (c) 2009, Ambroz Bizjak <ambrop7@gmail.com>
// https://github.com/ambrop72/badvpn
//
func (sshClient *sshClient) handleUDPChannel(newChannel ssh.NewChannel) {

	// Accept this channel immediately. This channel will replace any
	// previously existing UDP channel for this client.

	sshChannel, requests, err := newChannel.Accept()
	if err != nil {
		log.WithContextFields(LogFields{"error": err}).Warning("accept new channel failed")
		return
	}
	go ssh.DiscardRequests(requests)
	defer sshChannel.Close()

	sshClient.setUDPChannel(sshChannel)

	multiplexer := &udpPortForwardMultiplexer{
		sshClient:      sshClient,
		sshChannel:     sshChannel,
		portForwards:   make(map[uint16]*udpPortForward),
		portForwardLRU: psiphon.NewLRUConns(),
		relayWaitGroup: new(sync.WaitGroup),
	}
	multiplexer.run()
}

type udpPortForwardMultiplexer struct {
	sshClient         *sshClient
	sshChannel        ssh.Channel
	portForwardsMutex sync.Mutex
	portForwards      map[uint16]*udpPortForward
	relayWaitGroup    *sync.WaitGroup
	portForwardLRU    *psiphon.LRUConns
}

func (mux *udpPortForwardMultiplexer) run() {

	// In a loop, read udpgw messages from the client to this channel. Each message is
	// a UDP packet to send upstream either via a new port forward, or on an existing
	// port forward.
	//
	// A goroutine is run to read downstream packets for each UDP port forward. All read
	// packets are encapsulated in udpgw protocol and sent down the channel to the client.
	//
	// When the client disconnects or the server shuts down, the channel will close and
	// readUdpgwMessage will exit with EOF.

	buffer := make([]byte, udpgwProtocolMaxMessageSize)
	for {
		// Note: message.packet points to the reusable memory in "buffer".
		// Each readUdpgwMessage call will overwrite the last message.packet.
		message, err := readUdpgwMessage(mux.sshChannel, buffer)
		if err != nil {
			if err != io.EOF {
				log.WithContextFields(LogFields{"error": err}).Warning("readUpdgwMessage failed")
			}
			break
		}

		mux.portForwardsMutex.Lock()
		portForward := mux.portForwards[message.connID]
		mux.portForwardsMutex.Unlock()

		if portForward != nil && message.discardExistingConn {
			// The port forward's goroutine will complete cleanup, including
			// tallying stats and calling sshClient.closedPortForward.
			// portForward.conn.Close() will signal this shutdown.
			// TODO: wait for goroutine to exit before proceeding?
			portForward.conn.Close()
			portForward = nil
		}

		if portForward != nil {

			// Verify that portForward remote address matches latest message

			if 0 != bytes.Compare(portForward.remoteIP, message.remoteIP) ||
				portForward.remotePort != message.remotePort {

				log.WithContext().Warning("UDP port forward remote address mismatch")
				continue
			}

		} else {

			// Create a new port forward

			if !mux.sshClient.isPortForwardPermitted(
				int(message.remotePort),
				mux.sshClient.trafficRules.AllowUDPPorts,
				mux.sshClient.trafficRules.DenyUDPPorts) {
				// The udpgw protocol has no error response, so
				// we just discard the message and read another.
				continue
			}

			mux.sshClient.openedPortForward(mux.sshClient.udpTrafficState)
			// Note: can't defer sshClient.closedPortForward() here

			// TOCTOU note: important to increment the port forward count (via
			// openPortForward) _before_ checking isPortForwardLimitExceeded
			if mux.sshClient.isPortForwardLimitExceeded(
				mux.sshClient.tcpTrafficState,
				mux.sshClient.trafficRules.MaxUDPPortForwardCount) {

				// Close the oldest UDP port forward. CloseOldest() closes
				// the conn and the port forward's goroutine will complete
				// the cleanup asynchronously.
				//
				// See LRU comment in handleTCPChannel() for a known
				// limitations regarding CloseOldest().
				mux.portForwardLRU.CloseOldest()

				log.WithContextFields(
					LogFields{
						"maxCount": mux.sshClient.trafficRules.MaxUDPPortForwardCount,
					}).Debug("closed LRU UDP port forward")
			}

			dialIP := net.IP(message.remoteIP)
			dialPort := int(message.remotePort)

			// Transparent DNS forwarding
			if message.forwardDNS {
				dialIP, dialPort = mux.transparentDNSAddress(dialIP, dialPort)
			}

			log.WithContextFields(
				LogFields{
					"remoteAddr": fmt.Sprintf("%s:%d", dialIP.String(), dialPort),
					"connID":     message.connID}).Debug("dialing")

			// TODO: on EADDRNOTAVAIL, temporarily suspend new clients
			udpConn, err := net.DialUDP(
				"udp", nil, &net.UDPAddr{IP: dialIP, Port: dialPort})
			if err != nil {
				mux.sshClient.closedPortForward(mux.sshClient.udpTrafficState, 0, 0)
				log.WithContextFields(LogFields{"error": err}).Warning("DialUDP failed")
				continue
			}

			lruEntry := mux.portForwardLRU.Add(udpConn)

			// ActivityMonitoredConn monitors the TCP port forward I/O and updates
			// its LRU status. ActivityMonitoredConn also times out read on the port
			// forward if both reads and writes have been idle for the specified
			// duration.
			conn := psiphon.NewActivityMonitoredConn(
				udpConn,
				time.Duration(mux.sshClient.trafficRules.IdleUDPPortForwardTimeoutMilliseconds)*time.Millisecond,
				true,
				lruEntry)

			portForward = &udpPortForward{
				connID:       message.connID,
				preambleSize: message.preambleSize,
				remoteIP:     message.remoteIP,
				remotePort:   message.remotePort,
				conn:         conn,
				lruEntry:     lruEntry,
				bytesUp:      0,
				bytesDown:    0,
				mux:          mux,
			}
			mux.portForwardsMutex.Lock()
			mux.portForwards[portForward.connID] = portForward
			mux.portForwardsMutex.Unlock()

			// relayDownstream will call sshClient.closedPortForward()
			mux.relayWaitGroup.Add(1)
			go portForward.relayDownstream()
		}

		// Note: assumes UDP writes won't block (https://golang.org/pkg/net/#UDPConn.WriteToUDP)
		_, err = portForward.conn.Write(message.packet)
		if err != nil {
			// Debug since errors such as "write: operation not permitted" occur during normal operation
			log.WithContextFields(LogFields{"error": err}).Debug("upstream UDP relay failed")
			// The port forward's goroutine will complete cleanup
			portForward.conn.Close()
		}

		portForward.lruEntry.Touch()

		atomic.AddInt64(&portForward.bytesUp, int64(len(message.packet)))
	}

	// Cleanup all UDP port forward workers when exiting

	mux.portForwardsMutex.Lock()
	for _, portForward := range mux.portForwards {
		// The port forward's goroutine will complete cleanup
		portForward.conn.Close()
	}
	mux.portForwardsMutex.Unlock()

	mux.relayWaitGroup.Wait()
}

func (mux *udpPortForwardMultiplexer) transparentDNSAddress(
	dialIP net.IP, dialPort int) (net.IP, int) {

	if mux.sshClient.sshServer.config.UDPForwardDNSServerAddress != "" {
		// Note: UDPForwardDNSServerAddress is validated in LoadConfig
		host, portStr, _ := net.SplitHostPort(
			mux.sshClient.sshServer.config.UDPForwardDNSServerAddress)
		dialIP = net.ParseIP(host)
		dialPort, _ = strconv.Atoi(portStr)
	}
	return dialIP, dialPort
}

func (mux *udpPortForwardMultiplexer) removePortForward(connID uint16) {
	mux.portForwardsMutex.Lock()
	delete(mux.portForwards, connID)
	mux.portForwardsMutex.Unlock()
}

type udpPortForward struct {
	connID       uint16
	preambleSize int
	remoteIP     []byte
	remotePort   uint16
	conn         net.Conn
	lruEntry     *psiphon.LRUConnsEntry
	bytesUp      int64
	bytesDown    int64
	mux          *udpPortForwardMultiplexer
}

func (portForward *udpPortForward) relayDownstream() {
	defer portForward.mux.relayWaitGroup.Done()

	// Downstream UDP packets are read into the reusable memory
	// in "buffer" starting at the offset past the udpgw message
	// header and address, leaving enough space to write the udpgw
	// values into the same buffer and use for writing to the ssh
	// channel.
	//
	// Note: there is one downstream buffer per UDP port forward,
	// while for upstream there is one buffer per client.
	// TODO: is the buffer size larger than necessary?
	buffer := make([]byte, udpgwProtocolMaxMessageSize)
	packetBuffer := buffer[portForward.preambleSize:udpgwProtocolMaxMessageSize]
	for {
		// TODO: if read buffer is too small, excess bytes are discarded?
		packetSize, err := portForward.conn.Read(packetBuffer)
		if packetSize > udpgwProtocolMaxPayloadSize {
			err = fmt.Errorf("unexpected packet size: %d", packetSize)
		}
		if err != nil {
			if err != io.EOF {
				// Debug since errors such as "use of closed network connection" occur during normal operation
				log.WithContextFields(LogFields{"error": err}).Warning("downstream UDP relay failed")
			}
			break
		}

		err = writeUdpgwPreamble(
			portForward.preambleSize,
			portForward.connID,
			portForward.remoteIP,
			portForward.remotePort,
			uint16(packetSize),
			buffer)
		if err == nil {
			_, err = portForward.mux.sshChannel.Write(buffer[0 : portForward.preambleSize+packetSize])
		}

		if err != nil {
			// Close the channel, which will interrupt the main loop.
			portForward.mux.sshChannel.Close()
			log.WithContextFields(LogFields{"error": err}).Debug("downstream UDP relay failed")
			break
		}

		portForward.lruEntry.Touch()

		atomic.AddInt64(&portForward.bytesDown, int64(packetSize))
	}

	portForward.mux.removePortForward(portForward.connID)

	portForward.lruEntry.Remove()

	portForward.conn.Close()

	bytesUp := atomic.LoadInt64(&portForward.bytesUp)
	bytesDown := atomic.LoadInt64(&portForward.bytesDown)
	portForward.mux.sshClient.closedPortForward(
		portForward.mux.sshClient.udpTrafficState, bytesUp, bytesDown)

	log.WithContextFields(
		LogFields{
			"remoteAddr": fmt.Sprintf("%s:%d",
				net.IP(portForward.remoteIP).String(), portForward.remotePort),
			"bytesUp":   bytesUp,
			"bytesDown": bytesDown,
			"connID":    portForward.connID}).Debug("exiting")
}

// TODO: express and/or calculate udpgwProtocolMaxPayloadSize as function of MTU?
const (
	udpgwProtocolFlagKeepalive = 1 << 0
	udpgwProtocolFlagRebind    = 1 << 1
	udpgwProtocolFlagDNS       = 1 << 2
	udpgwProtocolFlagIPv6      = 1 << 3

	udpgwProtocolMaxPreambleSize = 23
	udpgwProtocolMaxPayloadSize  = 32768
	udpgwProtocolMaxMessageSize  = udpgwProtocolMaxPreambleSize + udpgwProtocolMaxPayloadSize
)

type udpProtocolMessage struct {
	connID              uint16
	preambleSize        int
	remoteIP            []byte
	remotePort          uint16
	discardExistingConn bool
	forwardDNS          bool
	packet              []byte
}

func readUdpgwMessage(
	reader io.Reader, buffer []byte) (*udpProtocolMessage, error) {

	// udpgw message layout:
	//
	// | 2 byte size | 3 byte header | 6 or 18 byte address | variable length packet |

	for {
		// Read message

		_, err := io.ReadFull(reader, buffer[0:2])
		if err != nil {
			return nil, psiphon.ContextError(err)
		}

		size := uint16(buffer[0]) + uint16(buffer[1])<<8

		if int(size) > len(buffer)-2 {
			return nil, psiphon.ContextError(errors.New("invalid udpgw message size"))
		}

		_, err = io.ReadFull(reader, buffer[2:2+size])
		if err != nil {
			return nil, psiphon.ContextError(err)
		}

		flags := buffer[2]

		connID := uint16(buffer[3]) + uint16(buffer[4])<<8

		// Ignore udpgw keep-alive messages -- read another message

		if flags&udpgwProtocolFlagKeepalive == udpgwProtocolFlagKeepalive {
			continue
		}

		// Read address

		var remoteIP []byte
		var remotePort uint16
		var packetStart, packetEnd int

		if flags&udpgwProtocolFlagIPv6 == udpgwProtocolFlagIPv6 {

			if size < 21 {
				return nil, psiphon.ContextError(errors.New("invalid udpgw message size"))
			}

			remoteIP = make([]byte, 16)
			copy(remoteIP, buffer[5:21])
			remotePort = uint16(buffer[21]) + uint16(buffer[22])<<8
			packetStart = 23
			packetEnd = 23 + int(size) - 2

		} else {

			if size < 9 {
				return nil, psiphon.ContextError(errors.New("invalid udpgw message size"))
			}

			remoteIP = make([]byte, 4)
			copy(remoteIP, buffer[5:9])
			remotePort = uint16(buffer[9]) + uint16(buffer[10])<<8
			packetStart = 11
			packetEnd = 11 + int(size) - 2
		}

		// Assemble message
		// Note: udpProtocolMessage.packet references memory in the input buffer

		message := &udpProtocolMessage{
			connID:              connID,
			preambleSize:        packetStart,
			remoteIP:            remoteIP,
			remotePort:          remotePort,
			discardExistingConn: flags&udpgwProtocolFlagRebind == udpgwProtocolFlagRebind,
			forwardDNS:          flags&udpgwProtocolFlagDNS == udpgwProtocolFlagDNS,
			packet:              buffer[packetStart:packetEnd],
		}

		return message, nil
	}
}

func writeUdpgwPreamble(
	preambleSize int,
	connID uint16,
	remoteIP []byte,
	remotePort uint16,
	packetSize uint16,
	buffer []byte) error {

	if preambleSize != 7+len(remoteIP) {
		return errors.New("invalid udpgw preamble size")
	}

	size := uint16(preambleSize-2) + packetSize

	// size
	buffer[0] = byte(size & 0xFF)
	buffer[1] = byte(size >> 8)

	// flags
	buffer[2] = 0

	// connID
	buffer[3] = byte(connID & 0xFF)
	buffer[4] = byte(connID >> 8)

	// addr
	copy(buffer[5:5+len(remoteIP)], remoteIP)
	buffer[5+len(remoteIP)] = byte(remotePort & 0xFF)
	buffer[6+len(remoteIP)] = byte(remotePort >> 8)

	return nil
}
