// Copyright 2022 Tailscale Inc & Contributors
// SPDX-License-Identifier: BSD-3-Clause

package golink

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	"tailscale.com/tstime"
)

// Link is the structure stored for each go short link.
type Link struct {
	Short    string // the "foo" part of http://go/foo
	Long     string // the target URL or text/template pattern to run
	Created  time.Time
	LastEdit time.Time // when the link was last edited
	Owner    string    // user@domain
}

// ClickStats is the number of clicks a set of links have received in a given
// time period. It is keyed by link short name, with values of total clicks.
type ClickStats map[string]int

// linkID returns the normalized ID for a link short name.
func linkID(short string) string {
	id := url.PathEscape(strings.ToLower(short))
	id = strings.ReplaceAll(id, "-", "")
	return id
}

// SQLiteDB stores Links in a SQLite database.
//
// It also maintains an in-memory index of all Links, kept in sync with the
// database under the single mutex below. The index exists to service
// low-latency lookups without issuing a SQL query per keystroke.
type SQLiteDB struct {
	db *sql.DB
	mu sync.RWMutex

	// idx is the in-memory index of all Links, keyed by linkID(Short).
	// It is populated on construction from LoadAll and kept in sync by
	// Save, SaveAll, and Delete. Link values in idx are never mutated
	// after insertion (callers of Save construct a fresh *Link each time),
	// so pointers may be read without copying.
	idx map[string]*Link

	clock tstime.Clock // allow overriding time for tests
}

//go:embed schema.sql
var sqlSchema string

// NewSQLiteDB returns a new SQLiteDB that stores links in a SQLite database stored at f.
func NewSQLiteDB(f string) (*SQLiteDB, error) {
	db, err := sql.Open("sqlite", f)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}

	if _, err = db.Exec(sqlSchema); err != nil {
		return nil, err
	}

	s := &SQLiteDB{db: db, idx: make(map[string]*Link)}
	// Populate the in-memory index from any pre-existing rows.
	if err := s.rebuildIndexLocked(); err != nil {
		return nil, fmt.Errorf("rebuilding in-memory link index: %w", err)
	}
	return s, nil
}

// rebuildIndexLocked populates s.idx from the Links table. It must be called
// with s.mu held for writing (or with no concurrent users, as in the
// constructor). We bypass LoadAll to avoid deadlocking on the mutex.
func (s *SQLiteDB) rebuildIndexLocked() error {
	rows, err := s.db.Query("SELECT Short, Long, Created, LastEdit, Owner FROM Links")
	if err != nil {
		return err
	}
	defer rows.Close()
	s.idx = make(map[string]*Link)
	for rows.Next() {
		link := new(Link)
		var created, lastEdit int64
		if err := rows.Scan(&link.Short, &link.Long, &created, &lastEdit, &link.Owner); err != nil {
			return err
		}
		link.Created = time.Unix(created, 0).UTC()
		link.LastEdit = time.Unix(lastEdit, 0).UTC()
		s.idx[linkID(link.Short)] = link
	}
	return rows.Err()
}

// Now returns the current time.
func (s *SQLiteDB) Now() time.Time {
	return tstime.DefaultClock{Clock: s.clock}.Now()
}

// LoadAll returns all stored Links.
//
// The caller owns the returned values.
func (s *SQLiteDB) LoadAll() ([]*Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var links []*Link
	rows, err := s.db.Query("SELECT Short, Long, Created, LastEdit, Owner FROM Links")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		link := new(Link)
		var created, lastEdit int64
		err := rows.Scan(&link.Short, &link.Long, &created, &lastEdit, &link.Owner)
		if err != nil {
			return nil, err
		}
		link.Created = time.Unix(created, 0).UTC()
		link.LastEdit = time.Unix(lastEdit, 0).UTC()
		links = append(links, link)
	}
	return links, rows.Err()
}

// Load returns a Link by its short name.
//
// It returns fs.ErrNotExist if the link does not exist.
//
// The caller owns the returned value.
func (s *SQLiteDB) Load(short string) (*Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	link := new(Link)
	var created, lastEdit int64
	row := s.db.QueryRow("SELECT Short, Long, Created, LastEdit, Owner FROM Links WHERE ID = ?1 LIMIT 1", linkID(short))
	err := row.Scan(&link.Short, &link.Long, &created, &lastEdit, &link.Owner)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = fs.ErrNotExist
		}
		return nil, err
	}
	link.Created = time.Unix(created, 0).UTC()
	link.LastEdit = time.Unix(lastEdit, 0).UTC()
	return link, nil
}

// Save saves a Link.
func (s *SQLiteDB) Save(link *Link) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec("INSERT OR REPLACE INTO Links (ID, Short, Long, Created, LastEdit, Owner) VALUES (?, ?, ?, ?, ?, ?)", linkID(link.Short), link.Short, link.Long, link.Created.Unix(), link.LastEdit.Unix(), link.Owner)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("expected to affect 1 row, affected %d", rows)
	}
	// Keep the in-memory index in sync. Store a copy so the caller
	// can't mutate the indexed value after the fact.
	cp := *link
	s.idx[linkID(link.Short)] = &cp
	return nil
}

// SaveAbsent saves multiple Links in a single transaction. Existing links with
// the same ID are not overwritten. It returns the number of rows that were
// newly inserted.
func (s *SQLiteDB) SaveAbsent(links []*Link) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(context.TODO(), nil)
	if err != nil {
		return 0, err
	}
	stmt, err := tx.Prepare("INSERT OR IGNORE INTO Links (ID, Short, Long, Created, LastEdit, Owner) VALUES (?, ?, ?, ?, ?, ?)")
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	defer stmt.Close()

	// Track which links were actually inserted (vs ignored) so we can
	// mirror the DB state into the in-memory index only after a
	// successful Commit.
	insertedLinks := make([]*Link, 0, len(links))
	for _, link := range links {
		result, err := stmt.Exec(linkID(link.Short), link.Short, link.Long, link.Created.Unix(), link.LastEdit.Unix(), link.Owner)
		if err != nil {
			tx.Rollback()
			return len(insertedLinks), err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			tx.Rollback()
			return len(insertedLinks), err
		}
		if rows == 1 {
			insertedLinks = append(insertedLinks, link)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	for _, link := range insertedLinks {
		// Only insert if absent so we don't stomp an entry that the DB
		// had already (INSERT OR IGNORE keeps the existing row).
		id := linkID(link.Short)
		if _, exists := s.idx[id]; !exists {
			cp := *link
			s.idx[id] = &cp
		}
	}
	return len(insertedLinks), nil
}

// Delete removes a Link using its short name.
func (s *SQLiteDB) Delete(short string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec("DELETE FROM Links WHERE ID = ?", linkID(short))
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("expected to affect 1 row, affected %d", rows)
	}
	delete(s.idx, linkID(short))
	return nil
}

// LoadStats returns click stats for links.
func (s *SQLiteDB) LoadStats() (ClickStats, error) {
	allLinks, err := s.LoadAll()
	if err != nil {
		return nil, err
	}
	linkmap := make(map[string]string, len(allLinks)) // map ID => Short
	for _, link := range allLinks {
		linkmap[linkID(link.Short)] = link.Short
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT ID, sum(Clicks) FROM Stats GROUP BY ID")
	if err != nil {
		return nil, err
	}
	stats := make(map[string]int)
	for rows.Next() {
		var id string
		var clicks int
		err := rows.Scan(&id, &clicks)
		if err != nil {
			return nil, err
		}
		short := linkmap[id]
		stats[short] = clicks
	}
	return stats, rows.Err()
}

// SaveStats records click stats for links.  The provided map includes
// incremental clicks that have occurred since the last time SaveStats
// was called.
func (s *SQLiteDB) SaveStats(stats ClickStats) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(context.TODO(), nil)
	if err != nil {
		return err
	}
	now := s.Now().Unix()
	for short, clicks := range stats {
		_, err := tx.Exec("INSERT INTO Stats (ID, Created, Clicks) VALUES (?, ?, ?)", linkID(short), now, clicks)
		if err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// DeleteStats deletes click stats for a link.
func (s *SQLiteDB) DeleteStats(short string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("DELETE FROM Stats WHERE ID = ?", linkID(short))
	if err != nil {
		return err
	}
	return nil
}

// GetLinksByOwner returns all Links owned by the specified owner.
func (s *SQLiteDB) GetLinksByOwner(owner string) ([]*Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var links []*Link
	rows, err := s.db.Query("SELECT Short, Long, Created, LastEdit, Owner FROM Links WHERE LOWER(Owner) = LOWER(?)", owner)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		link := new(Link)
		var created, lastEdit int64
		err := rows.Scan(&link.Short, &link.Long, &created, &lastEdit, &link.Owner)
		if err != nil {
			return nil, err
		}
		link.Created = time.Unix(created, 0).UTC()
		link.LastEdit = time.Unix(lastEdit, 0).UTC()
		links = append(links, link)
	}
	return links, rows.Err()
}
