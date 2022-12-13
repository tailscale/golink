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
	"time"

	_ "modernc.org/sqlite"
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
type SQLiteDB struct {
	db *sql.DB
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

	return &SQLiteDB{db: db}, nil
}

// LoadAll returns all stored Links.
//
// The caller owns the returned values.
func (s *SQLiteDB) LoadAll() ([]*Link, error) {
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
	tx, err := s.db.BeginTx(context.TODO(), nil)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	for short, clicks := range stats {
		_, err := tx.Exec("INSERT INTO Stats (ID, Created, Clicks) VALUES (?, ?, ?)", linkID(short), now, clicks)
		if err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
