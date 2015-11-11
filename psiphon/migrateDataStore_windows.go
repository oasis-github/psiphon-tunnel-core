// TODO: Windows only build flag + runtime.GOOS check in datastore.go?

/*
 * Copyright (c) 2015, Psiphon Inc.
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

package psiphon

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/Psiphon-Inc/go-sqlite3"
)

var legacyDb *sql.DB

func prepareMigrationEntries(config *Config) ([]*ServerEntry, error) {
	// If DATA_STORE_FILENAME does not exist on disk
	if _, err := os.Stat(filepath.Join(config.DataStoreDirectory, DATA_STORE_FILENAME)); os.IsNotExist(err) {
		// If LEGACY_DATA_STORE_FILENAME exists on disk
		if _, err := os.Stat(filepath.Join(config.DataStoreDirectory, LEGACY_DATA_STORE_FILENAME)); err == nil {
			var migratableServerEntries []*ServerEntry

			legacyDb, err = sql.Open("sqlite3", fmt.Sprintf("file:%s?cache=private&mode=rwc", filepath.Join(config.DataStoreDirectory, LEGACY_DATA_STORE_FILENAME)))
			defer legacyDb.Close()

			if err != nil {
				return migratableServerEntries, err
			}

			initialization := "pragma journal_mode=WAL;\n"
			_, err = legacyDb.Exec(initialization)
			if err != nil {
				return migratableServerEntries, err
			}

			iterator, err := newlegacyServerEntryIterator(config)
			if err != nil {
				return migratableServerEntries, err
			}
			defer iterator.Close()

			for {
				serverEntry, err := iterator.Next()
				if err != nil {
					err = fmt.Errorf("failed to iterate legacy server entries: %s", err)
					break
				}
				if serverEntry == nil {
					break
				}

				migratableServerEntries = append(migratableServerEntries, serverEntry)
			}
			NoticeInfo("%d server entries prepared for data store migration", len(migratableServerEntries))
		}
	}

	return migratableServerEntries, nil
}

// migrateEntries calls the BoltDB data store method to shuffle
// and store an array of server entries (StoreServerEntries)
// Failing to migrate entries, or delete the legacy file is never fatal
func migrateEntries(serverEntries []*ServerEntry, legacyDataStoreFilename string) {
	checkInitDataStore()

	err := StoreServerEntries(serverEntries, false)
	if err != nil {
		NoticeAlert("failed to store migrated server entries: %s", err)
	} else {
		// Retain server affinity from old datastore by taking the first
		// array element (previous top ranked server) and promoting it
		// to the top rank before the server selection process begins
		err = PromoteServerEntry(serverEntries[0].IpAddress)
		if err != nil {
			NoticeAlert("failed to promote server entry: %s", err)
		}

		NoticeAlert("%d server entries successfully migrated to new data store", len(serverEntries))
	}

	err = os.Remove(legacyDataStoreFilename)
	if err != nil {
		NoticeAlert("failed to delete legacy data store file '%s': %s", legacyDataStoreFilename, err)
	}

	return
}

// This code is copied from the dataStore.go code used to operate the legacy
// SQLite datastore. The word "legacy" was added to all of the method names to avoid
// namespace conflicts with the methods used to operate the BoltDB datastore

// legacyServerEntryIterator is used to iterate over
// stored server entries in rank order.
type legacyServerEntryIterator struct {
	region            string
	protocol          string
	shuffleHeadLength int
	transaction       *sql.Tx
	cursor            *sql.Rows
}

// newLegacyServerEntryIterator creates a new legacyServerEntryIterator
func newlegacyServerEntryIterator(config *Config) (iterator *legacyServerEntryIterator, err error) {

	iterator = &legacyServerEntryIterator{
		region:            config.EgressRegion,
		protocol:          config.TunnelProtocol,
		shuffleHeadLength: config.TunnelPoolSize,
	}
	err = iterator.Reset()
	if err != nil {
		return nil, err
	}
	return iterator, nil
}

// Close cleans up resources associated with a legacyServerEntryIterator.
func (iterator *legacyServerEntryIterator) Close() {
	if iterator.cursor != nil {
		iterator.cursor.Close()
	}
	iterator.cursor = nil
	if iterator.transaction != nil {
		iterator.transaction.Rollback()
	}
	iterator.transaction = nil
}

// Next returns the next server entry, by rank, for a legacyServerEntryIterator.
// Returns nil with no error when there is no next item.
func (iterator *legacyServerEntryIterator) Next() (serverEntry *ServerEntry, err error) {
	defer func() {
		if err != nil {
			iterator.Close()
		}
	}()

	if !iterator.cursor.Next() {
		err = iterator.cursor.Err()
		if err != nil {
			return nil, ContextError(err)
		}
		// There is no next item
		return nil, nil
	}

	var data []byte
	err = iterator.cursor.Scan(&data)
	if err != nil {
		return nil, ContextError(err)
	}
	serverEntry = new(ServerEntry)
	err = json.Unmarshal(data, serverEntry)
	if err != nil {
		return nil, ContextError(err)
	}

	return MakeCompatibleServerEntry(serverEntry), nil
}

// Reset a NewlegacyServerEntryIterator to the start of its cycle. The next
// call to Next will return the first server entry.
func (iterator *legacyServerEntryIterator) Reset() error {
	iterator.Close()

	count := countLegacyServerEntries(iterator.region, iterator.protocol)
	NoticeCandidateServers(iterator.region, iterator.protocol, count)

	transaction, err := legacyDb.Begin()
	if err != nil {
		return ContextError(err)
	}
	var cursor *sql.Rows

	// This query implements the Psiphon server candidate selection
	// algorithm: the first TunnelPoolSize server candidates are in rank
	// (priority) order, to favor previously successful servers; then the
	// remaining long tail is shuffled to raise up less recent candidates.

	whereClause, whereParams := makeServerEntryWhereClause(
		iterator.region, iterator.protocol, nil)
	headLength := iterator.shuffleHeadLength
	queryFormat := `
		select data from serverEntry %s
		order by case
		when rank > coalesce((select rank from serverEntry %s order by rank desc limit ?, 1), -1) then rank
		else abs(random())%%((select rank from serverEntry %s order by rank desc limit ?, 1))
		end desc;`
	query := fmt.Sprintf(queryFormat, whereClause, whereClause, whereClause)
	params := make([]interface{}, 0)
	params = append(params, whereParams...)
	params = append(params, whereParams...)
	params = append(params, headLength)
	params = append(params, whereParams...)
	params = append(params, headLength)

	cursor, err = transaction.Query(query, params...)
	if err != nil {
		transaction.Rollback()
		return ContextError(err)
	}
	iterator.transaction = transaction
	iterator.cursor = cursor
	return nil
}

func makeServerEntryWhereClause(
	region, protocol string, excludeIds []string) (whereClause string, whereParams []interface{}) {
	whereClause = ""
	whereParams = make([]interface{}, 0)
	if region != "" {
		whereClause += " where region = ?"
		whereParams = append(whereParams, region)
	}
	if protocol != "" {
		if len(whereClause) > 0 {
			whereClause += " and"
		} else {
			whereClause += " where"
		}
		whereClause +=
			" exists (select 1 from serverEntryProtocol where protocol = ? and serverEntryId = serverEntry.id)"
		whereParams = append(whereParams, protocol)
	}
	if len(excludeIds) > 0 {
		if len(whereClause) > 0 {
			whereClause += " and"
		} else {
			whereClause += " where"
		}
		whereClause += " id in ("
		for index, id := range excludeIds {
			if index > 0 {
				whereClause += ", "
			}
			whereClause += "?"
			whereParams = append(whereParams, id)
		}
		whereClause += ")"
	}
	return whereClause, whereParams
}

// countLegacyServerEntries returns a count of stored servers for the specified region and protocol.
func countLegacyServerEntries(region, protocol string) int {
	var count int
	whereClause, whereParams := makeServerEntryWhereClause(region, protocol, nil)
	query := "select count(*) from serverEntry" + whereClause
	err := legacyDb.QueryRow(query, whereParams...).Scan(&count)

	if err != nil {
		NoticeAlert("countLegacyServerEntries failed: %s", err)
		return 0
	}

	if region == "" {
		region = "(any)"
	}
	if protocol == "" {
		protocol = "(any)"
	}
	NoticeInfo("servers for region %s and protocol %s: %d",
		region, protocol, count)

	return count
}