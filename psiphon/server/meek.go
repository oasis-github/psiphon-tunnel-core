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
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon"
	"golang.org/x/crypto/nacl/box"
)

// MeekServer is based on meek-server.go from Tor and Psiphon:
//
// https://gitweb.torproject.org/pluggable-transports/meek.git/blob/HEAD:/meek-client/meek-client.go
// CC0 1.0 Universal
//
// https://bitbucket.org/psiphon/psiphon-circumvention-system/src/default/go/meek-client/meek-client.go

// Protocol version 1 clients can handle arbitrary length response bodies. Older clients
// report no version number and expect at most 64K response bodies.
const MEEK_PROTOCOL_VERSION_1 = 1

// Protocol version 2 clients initiate a session by sending a encrypted and obfuscated meek
// cookie with their initial HTTP request. Connection information is contained within the
// encrypted cookie payload. The server inspects the cookie and establishes a new session and
// returns a new random session ID back to client via Set-Cookie header. The client uses this
// session ID on all subsequent requests for the remainder of the session.
const MEEK_PROTOCOL_VERSION_2 = 2

// TODO: protocol version 3, to support rapid shutdown of meek connections. Currently, there's no
// signal from the clients that the payload within meek is EOF and that a given request is the
// last request for a session; instead, session expiry is always what closes a session.

const MEEK_MAX_PAYLOAD_LENGTH = 0x10000
const MEEK_TURN_AROUND_TIMEOUT = 20 * time.Millisecond
const MEEK_EXTENDED_TURN_AROUND_TIMEOUT = 100 * time.Millisecond
const MEEK_MAX_SESSION_STALENESS = 45 * time.Second
const MEEK_HTTP_CLIENT_READ_TIMEOUT = 45 * time.Second
const MEEK_HTTP_CLIENT_WRITE_TIMEOUT = 10 * time.Second
const MEEK_MIN_SESSION_ID_LENGTH = 8
const MEEK_MAX_SESSION_ID_LENGTH = 20

// MeekServer implements the meek protocol, which tunnels TCP traffic (in the case of Psiphon,
// Obfusated SSH traffic) over HTTP. Meek may be fronted (through a CDN) or direct and may be
// HTTP or HTTPS.
//
// Upstream traffic arrives in HTTP request bodies and downstream traffic is sent in response
// bodies. The sequence of traffic for a given flow is associated using a session ID that's
// set as a HTTP cookie for the client to submit with each request.
//
// MeekServer hooks into TunnelServer via the net.Conn interface by transforming the
// HTTP payload traffic for a given session into net.Conn conforming Read()s and Write()s via
// the meekConn struct.
type MeekServer struct {
	config        *Config
	listener      net.Listener
	tlsConfig     *tls.Config
	clientHandler func(clientConn net.Conn)
	openConns     *psiphon.Conns
	stopBroadcast <-chan struct{}
	sessionsLock  sync.RWMutex
	sessions      map[string]*meekSession
}

// NewMeekServer initializes a new meek server.
func NewMeekServer(
	config *Config,
	listener net.Listener,
	useTLS bool,
	clientHandler func(clientConn net.Conn),
	stopBroadcast <-chan struct{}) (*MeekServer, error) {

	meekServer := &MeekServer{
		config:        config,
		listener:      listener,
		clientHandler: clientHandler,
		openConns:     new(psiphon.Conns),
		stopBroadcast: stopBroadcast,
		sessions:      make(map[string]*meekSession),
	}

	if useTLS {
		tlsConfig, err := makeMeekTLSConfig(config)
		if err != nil {
			return nil, psiphon.ContextError(err)
		}
		meekServer.tlsConfig = tlsConfig
	}

	return meekServer, nil
}

// Run runs the meek server; this function blocks while serving HTTP or
// HTTPS connections on the specified listener. This function also runs
// a goroutine which cleans up expired meek client sessions.
//
// To stop the meek server, both Close() the listener and set the stopBroadcast
// signal specified in NewMeekServer.
func (server *MeekServer) Run() error {
	defer server.listener.Close()
	defer server.openConns.CloseAll()

	// Expire sessions

	reaperWaitGroup := new(sync.WaitGroup)
	reaperWaitGroup.Add(1)
	go func() {
		defer reaperWaitGroup.Done()
		ticker := time.NewTicker(MEEK_MAX_SESSION_STALENESS / 2)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				server.closeExpireSessions()
			case <-server.stopBroadcast:
				return
			}
		}
	}()

	// Serve HTTP or HTTPS

	httpServer := &http.Server{
		ReadTimeout:  MEEK_HTTP_CLIENT_READ_TIMEOUT,
		WriteTimeout: MEEK_HTTP_CLIENT_WRITE_TIMEOUT,
		Handler:      server,
		ConnState:    server.httpConnStateCallback,

		// Disable auto HTTP/2 (https://golang.org/doc/go1.6)
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
	}

	// Note: Serve() will be interrupted by listener.Close() call
	var err error
	if server.tlsConfig != nil {
		httpServer.TLSConfig = server.tlsConfig
		httpsServer := psiphon.HTTPSServer{Server: *httpServer}
		err = httpsServer.ServeTLS(server.listener)
	} else {
		err = httpServer.Serve(server.listener)
	}

	// Can't check for the exact error that Close() will cause in Accept(),
	// (see: https://code.google.com/p/go/issues/detail?id=4373). So using an
	// explicit stop signal to stop gracefully.
	select {
	case <-server.stopBroadcast:
		err = nil
	default:
	}

	reaperWaitGroup.Wait()

	return err
}

// ServeHTTP handles meek client HTTP requests, where the request body
// contains upstream traffic and the response will contain downstream
// traffic.
func (server *MeekServer) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {

	// Note: no longer requiring that the request method is POST

	// Check for the expected meek/session ID cookie.
	// Also check for prohibited HTTP headers.

	var meekCookie *http.Cookie
	for _, c := range request.Cookies() {
		meekCookie = c
		break
	}
	if meekCookie == nil || len(meekCookie.Value) == 0 {
		log.WithContext().Warning("missing meek cookie")
		server.terminateConnection(responseWriter, request)
		return
	}

	if len(server.config.MeekProhibitedHeaders) > 0 {
		for _, header := range server.config.MeekProhibitedHeaders {
			value := request.Header.Get(header)
			if header != "" {
				log.WithContextFields(LogFields{
					"header": header,
					"value":  value,
				}).Warning("prohibited meek header")
				server.terminateConnection(responseWriter, request)
				return
			}
		}
	}

	// Lookup or create a new session for given meek cookie/session ID.

	sessionID, session, err := server.getSession(request, meekCookie)
	if err != nil {
		log.WithContextFields(LogFields{"error": err}).Warning("session lookup failed")
		server.terminateConnection(responseWriter, request)
		return
	}

	// PumpReads causes a TunnelServer/SSH goroutine blocking on a Read to
	// read the request body as upstream traffic.
	// TODO: run PumpReads and PumpWrites concurrently?

	err = session.clientConn.PumpReads(request.Body)
	if err != nil {
		if err != io.EOF {
			log.WithContextFields(LogFields{"error": err}).Warning("pump reads failed")
		}
		server.terminateConnection(responseWriter, request)
		server.closeSession(sessionID)
		return
	}

	// Set cookie before writing the response.

	if session.meekProtocolVersion >= MEEK_PROTOCOL_VERSION_2 && session.sessionIDSent == false {
		// Replace the meek cookie with the session ID.
		// SetCookie for the the session ID cookie is only set once, to reduce overhead. This
		// session ID value replaces the original meek cookie value.
		http.SetCookie(responseWriter, &http.Cookie{Name: meekCookie.Name, Value: sessionID})
		session.sessionIDSent = true
	}

	// PumpWrites causes a TunnelServer/SSH goroutine blocking on a Write to
	// write its downstream traffic through to the response body.

	err = session.clientConn.PumpWrites(responseWriter)
	if err != nil {
		if err != io.EOF {
			log.WithContextFields(LogFields{"error": err}).Warning("pump writes failed")
		}
		server.terminateConnection(responseWriter, request)
		server.closeSession(sessionID)
		return
	}
}

// getSession returns the meek client session corresponding the
// meek cookie/session ID. If no session is found, the cookie is
// treated as a meek cookie for a new session and its payload is
// extracted and used to establish a new session.
func (server *MeekServer) getSession(
	request *http.Request, meekCookie *http.Cookie) (string, *meekSession, error) {

	// Check for an existing session

	server.sessionsLock.RLock()
	existingSessionID := meekCookie.Value
	session, ok := server.sessions[existingSessionID]
	server.sessionsLock.RUnlock()
	if ok {
		session.touch()
		return existingSessionID, session, nil
	}

	// TODO: can multiple http client connections using same session cookie
	// cause race conditions on session struct?

	// The session is new (or expired). Treat the cookie value as a new meek
	// cookie, extract the payload, and create a new session.

	payloadJSON, err := getMeekCookiePayload(server.config, meekCookie.Value)
	if err != nil {
		return "", nil, psiphon.ContextError(err)
	}

	// Note: this meek server ignores all but Version MeekProtocolVersion;
	// the other values are legacy or currently unused.
	var clientSessionData struct {
		MeekProtocolVersion    int    `json:"v"`
		PsiphonClientSessionId string `json:"s"`
		PsiphonServerAddress   string `json:"p"`
	}

	err = json.Unmarshal(payloadJSON, &clientSessionData)
	if err != nil {
		return "", nil, psiphon.ContextError(err)
	}

	// Determine the client remote address, which is used for geolocation
	// and stats. When an intermediate proxy of CDN is in use, we may be
	// able to determine the original client address by inspecting HTTP
	// headers such as X-Forwarded-For.

	clientIP := strings.Split(request.RemoteAddr, ":")[0]

	if len(server.config.MeekProxyForwardedForHeaders) > 0 {
		for _, header := range server.config.MeekProxyForwardedForHeaders {
			value := request.Header.Get(header)
			if len(value) > 0 {
				// Some headers, such as X-Forwarded-For, are a comma-separated
				// list of IPs (each proxy in a chain). The first IP should be
				// the client IP.
				proxyClientIP := strings.Split(header, ",")[0]
				if net.ParseIP(clientIP) != nil {
					clientIP = proxyClientIP
					break
				}
			}
		}
	}

	// Create a new meek conn that will relay the payload
	// between meek request/responses and the tunnel server client
	// handler. The client IP is also used to initialize the
	// meek conn with a useful value to return when the tunnel
	// server calls conn.RemoteAddr() to get the client's IP address.

	// Assumes clientIP is a value IP address; the port value is a stub
	// and is expected to be ignored.
	clientConn := newMeekConn(
		&net.TCPAddr{
			IP:   net.ParseIP(clientIP),
			Port: 0,
		},
		clientSessionData.MeekProtocolVersion)

	session = &meekSession{
		clientConn:          clientConn,
		meekProtocolVersion: clientSessionData.MeekProtocolVersion,
		sessionIDSent:       false,
	}
	session.touch()

	// Note: MEEK_PROTOCOL_VERSION_1 doesn't support changing the
	// meek cookie to a session ID; v1 clients always send the
	// original meek cookie value with each request. The issue with
	// v1 is that clients which wake after a device sleep will attempt
	// to resume a meek session and the server can't differentiate
	// between resuming a session and creating a new session. This
	// causes the v1 client connection to hang/timeout.
	sessionID := meekCookie.Value
	if clientSessionData.MeekProtocolVersion >= MEEK_PROTOCOL_VERSION_2 {
		sessionID, err = makeMeekSessionID()
		if err != nil {
			return "", nil, psiphon.ContextError(err)
		}
	}

	server.sessionsLock.Lock()
	server.sessions[sessionID] = session
	server.sessionsLock.Unlock()

	// Note: from the tunnel server's perspective, this client connection
	// will close when closeSessionHelper calls Close() on the meekConn.
	server.clientHandler(session.clientConn)

	return sessionID, session, nil
}

func (server *MeekServer) closeSessionHelper(
	sessionID string, session *meekSession) {

	// TODO: close the persistent HTTP client connection, if one exists
	session.clientConn.Close()
	// Note: assumes caller holds lock on sessionsLock
	delete(server.sessions, sessionID)
}

func (server *MeekServer) closeSession(sessionID string) {
	server.sessionsLock.Lock()
	session, ok := server.sessions[sessionID]
	if ok {
		server.closeSessionHelper(sessionID, session)
	}
	server.sessionsLock.Unlock()
}

func (server *MeekServer) closeExpireSessions() {
	server.sessionsLock.Lock()
	for sessionID, session := range server.sessions {
		if session.expired() {
			server.closeSessionHelper(sessionID, session)
		}
	}
	server.sessionsLock.Unlock()
}

// httpConnStateCallback tracks open persistent HTTP/HTTPS connections to the
// meek server.
func (server *MeekServer) httpConnStateCallback(conn net.Conn, connState http.ConnState) {
	switch connState {
	case http.StateNew:
		server.openConns.Add(conn)
	case http.StateHijacked, http.StateClosed:
		server.openConns.Remove(conn)
	}
}

// terminateConnection sends a 404 response to a client and also closes
// a persisitent connection.
func (server *MeekServer) terminateConnection(
	responseWriter http.ResponseWriter, request *http.Request) {

	http.NotFound(responseWriter, request)

	hijack, ok := responseWriter.(http.Hijacker)
	if !ok {
		return
	}
	conn, buffer, err := hijack.Hijack()
	if err != nil {
		return
	}
	buffer.Flush()
	conn.Close()
}

type meekSession struct {
	clientConn          *meekConn
	meekProtocolVersion int
	sessionIDSent       bool
	lastActivity        int64
}

func (session *meekSession) touch() {
	atomic.StoreInt64(&session.lastActivity, time.Now().UnixNano())
}

func (session *meekSession) expired() bool {
	lastActivity := atomic.LoadInt64(&session.lastActivity)
	return time.Since(time.Unix(0, lastActivity)) > MEEK_MAX_SESSION_STALENESS
}

// makeMeekTLSConfig creates a TLS config for a meek HTTPS listener.
// Currently, this config is optimized for fronted meek where the nature
// of the connection is non-circumvention; it's optimized for performance
// assuming the peer is an uncensored CDN.
func makeMeekTLSConfig(config *Config) (*tls.Config, error) {

	certificate, privateKey, err := GenerateWebServerCertificate(
		config.MeekCertificateCommonName)
	if err != nil {
		return nil, psiphon.ContextError(err)
	}

	tlsCertificate, err := tls.X509KeyPair(
		[]byte(certificate), []byte(privateKey))
	if err != nil {
		return nil, psiphon.ContextError(err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{tlsCertificate},
		NextProtos:   []string{"http/1.1"},
		MinVersion:   tls.VersionTLS10,

		// This is a reordering of the supported CipherSuites in golang 1.6. Non-ephemeral key
		// CipherSuites greatly reduce server load, and we try to select these since the meek
		// protocol is providing obfuscation, not privacy/integrity (this is provided by the
		// tunneled SSH), so we don't benefit from the perfect forward secrecy property provided
		// by ephemeral key CipherSuites.
		// https://github.com/golang/go/blob/1cb3044c9fcd88e1557eca1bf35845a4108bc1db/src/crypto/tls/cipher_suites.go#L75
		CipherSuites: []uint16{
			tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_RSA_WITH_RC4_128_SHA,
			tls.TLS_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_RSA_WITH_AES_256_CBC_SHA,
			tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_RC4_128_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_RC4_128_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA,
		},
		PreferServerCipherSuites: true,
	}, nil
}

// getMeekCookiePayload extracts the payload from a meek cookie. The cookie
// paylod is base64 encoded, obfuscated, and NaCl encrypted.
func getMeekCookiePayload(config *Config, cookieValue string) ([]byte, error) {
	decodedValue, err := base64.StdEncoding.DecodeString(cookieValue)
	if err != nil {
		return nil, psiphon.ContextError(err)
	}

	// The data consists of an obfuscated seed message prepended
	// to the obfuscated, encrypted payload. The server obfuscator
	// will read the seed message, leaving the remaining encrypted
	// data in the reader.

	reader := bytes.NewReader(decodedValue[:])

	obfuscator, err := psiphon.NewServerObfuscator(
		reader,
		&psiphon.ObfuscatorConfig{Keyword: config.MeekObfuscatedKey})
	if err != nil {
		return nil, psiphon.ContextError(err)
	}

	offset, err := reader.Seek(0, 1)
	if err != nil {
		return nil, psiphon.ContextError(err)
	}
	encryptedPayload := decodedValue[offset:]

	obfuscator.ObfuscateClientToServer(encryptedPayload)

	var nonce [24]byte
	var privateKey, ephemeralPublicKey [32]byte

	decodedPrivateKey, err := base64.StdEncoding.DecodeString(config.MeekCookieEncryptionPrivateKey)
	if err != nil {
		return nil, psiphon.ContextError(err)
	}
	copy(privateKey[:], decodedPrivateKey)

	if len(encryptedPayload) < 32 {
		return nil, psiphon.ContextError(errors.New("unexpected encrypted payload size"))
	}
	copy(ephemeralPublicKey[0:32], encryptedPayload[0:32])

	payload, ok := box.Open(nil, encryptedPayload[32:], &nonce, &ephemeralPublicKey, &privateKey)
	if !ok {
		return nil, psiphon.ContextError(errors.New("open box failed"))
	}

	return payload, nil
}

// makeMeekSessionID creates a new session ID. The variable size is intended to
// frustrate traffic analysis of both plaintext and TLS meek traffic.
func makeMeekSessionID() (string, error) {
	size := MEEK_MIN_SESSION_ID_LENGTH
	n, err := psiphon.MakeSecureRandomInt(MEEK_MAX_SESSION_ID_LENGTH - MEEK_MIN_SESSION_ID_LENGTH)
	if err != nil {
		return "", psiphon.ContextError(err)
	}
	size += n
	sessionID, err := psiphon.MakeRandomStringBase64(size)
	if err != nil {
		return "", psiphon.ContextError(err)
	}
	return sessionID, nil
}

// meekConn implements the net.Conn interface and is to be used as a client
// connection by the tunnel server (being passed to sshServer.handleClient).
// meekConn doesn't perform any real I/O, but instead shuttles io.Readers and
// io.Writers between goroutines blocking on Read()s and Write()s.
type meekConn struct {
	remoteAddr      net.Addr
	protocolVersion int
	closeBroadcast  chan struct{}
	closed          int32
	readLock        sync.Mutex
	readyReader     chan io.Reader
	readResult      chan error
	writeLock       sync.Mutex
	nextWriteBuffer chan []byte
	writeResult     chan error
}

func newMeekConn(remoteAddr net.Addr, protocolVersion int) *meekConn {
	return &meekConn{
		remoteAddr:      remoteAddr,
		protocolVersion: protocolVersion,
		closeBroadcast:  make(chan struct{}),
		closed:          0,
		readyReader:     make(chan io.Reader, 1),
		readResult:      make(chan error, 1),
		nextWriteBuffer: make(chan []byte, 1),
		writeResult:     make(chan error, 1),
	}
}

// PumpReads causes goroutines blocking on meekConn.Read() to read
// from the specified reader. This function blocks until the reader
// is fully consumed or the meekConn is closed.
// Note: channel scheme assumes only one concurrent call to PumpReads
func (conn *meekConn) PumpReads(reader io.Reader) error {

	// Assumes that readyReader won't block.
	conn.readyReader <- reader

	// Receiving readResult means Read(s) have consumed the
	// reader sent to readyReader. readyReader is now empty and
	// no reference is kept to the reader.
	select {
	case err := <-conn.readResult:
		return err
	case <-conn.closeBroadcast:
		return io.EOF
	}
}

// Read reads from the meekConn into buffer. Read blocks until
// some data is read or the meekConn closes. Under the hood, it
// waits for PumpReads to submit a reader to read from.
// Note: lock is to conform with net.Conn concurrency semantics
func (conn *meekConn) Read(buffer []byte) (int, error) {
	conn.readLock.Lock()
	defer conn.readLock.Unlock()

	var reader io.Reader
	select {
	case reader = <-conn.readyReader:
	case <-conn.closeBroadcast:
		return 0, io.EOF
	}

	n, err := reader.Read(buffer)

	if err != nil {
		if err == io.EOF {
			err = nil
		}
		// Assumes readerResult won't block.
		conn.readResult <- err
	} else {
		// There may be more data in the reader, but the caller's
		// buffer is full, so put the reader back into the ready
		// channel. PumpReads remains blocked waiting for another
		// Read call.
		// Note that the reader could be at EOF, while another call is
		// required to get that result (https://golang.org/pkg/io/#Reader).
		conn.readyReader <- reader
	}

	return n, err
}

// PumpReads causes goroutines blocking on meekConn.Write() to write
// to the specified writer. This function blocks until the meek response
// body limits (size for protocol v1, turn around time for protocol v2+)
// are met, or the meekConn is closed.
// Note: channel scheme assumes only one concurrent call to PumpWrites
func (conn *meekConn) PumpWrites(writer io.Writer) error {

	startTime := time.Now()
	timeout := time.NewTimer(MEEK_TURN_AROUND_TIMEOUT)
	defer timeout.Stop()

	for {
		select {
		case buffer := <-conn.nextWriteBuffer:
			_, err := writer.Write(buffer)

			// Assumes that writeResult won't block.
			// Note: always send the err to writeResult,
			// as the Write() caller is blocking on this.
			conn.writeResult <- err

			if err != nil {
				return err
			}

			if conn.protocolVersion < MEEK_PROTOCOL_VERSION_2 {
				// Protocol v1 clients expect at most
				// MEEK_MAX_PAYLOAD_LENGTH response bodies
				return nil
			}
			totalElapsedTime := time.Now().Sub(startTime) / time.Millisecond
			if totalElapsedTime >= MEEK_EXTENDED_TURN_AROUND_TIMEOUT {
				return nil
			}
			timeout.Reset(MEEK_TURN_AROUND_TIMEOUT)
		case <-timeout.C:
			return nil
		case <-conn.closeBroadcast:
			return io.EOF
		}
	}
}

// Write writes the buffer to the meekConn. It blocks until the
// entire buffer is written to or the meekConn closes. Under the
// hood, it waits for sufficient PumpWrites calls to consume the
// write buffer.
// Note: lock is to conform with net.Conn concurrency semantics
func (conn *meekConn) Write(buffer []byte) (int, error) {
	conn.writeLock.Lock()
	defer conn.writeLock.Unlock()

	// TODO: may be more efficient to send whole buffer
	// and have PumpWrites stash partial buffer when can't
	// send it all.

	n := 0
	for n < len(buffer) {
		end := n + MEEK_MAX_PAYLOAD_LENGTH
		if end > len(buffer) {
			end = len(buffer)
		}

		// Only write MEEK_MAX_PAYLOAD_LENGTH at a time,
		// to ensure compatibility with v1 protocol.
		chunk := buffer[n:end]

		select {
		case conn.nextWriteBuffer <- chunk:
		case <-conn.closeBroadcast:
			return n, io.EOF
		}

		// Wait for the buffer to be processed.
		select {
		case err := <-conn.writeResult:
			if err != nil {
				return n, err
			}
		case <-conn.closeBroadcast:
			return n, io.EOF
		}
		n += len(chunk)
	}
	return n, nil
}

// Close closes the meekConn. This will interrupt any blocked
// Read, Write, PumpReads, and PumpWrites.
func (conn *meekConn) Close() error {
	if atomic.CompareAndSwapInt32(&conn.closed, 0, 1) {
		close(conn.closeBroadcast)
	}
	return nil
}

// Stub implementation of net.Conn.LocalAddr
func (conn *meekConn) LocalAddr() net.Addr {
	return nil
}

// RemoteAddr returns the remoteAddr specified in newMeekConn. This
// acts as a proxy for the actual remote address, which is either a
// direct HTTP/HTTPS connection remote address, or in the case of
// downstream proxy of CDN fronts, some other value determined via
// HTTP headers.
func (conn *meekConn) RemoteAddr() net.Addr {
	return conn.remoteAddr
}

// Stub implementation of net.Conn.SetDeadline
func (conn *meekConn) SetDeadline(t time.Time) error {
	return psiphon.ContextError(errors.New("not supported"))
}

// Stub implementation of net.Conn.SetReadDeadline
func (conn *meekConn) SetReadDeadline(t time.Time) error {
	return psiphon.ContextError(errors.New("not supported"))
}

// Stub implementation of net.Conn.SetWriteDeadline
func (conn *meekConn) SetWriteDeadline(t time.Time) error {
	return psiphon.ContextError(errors.New("not supported"))
}
