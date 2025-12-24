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

//go:embed schema.sql
var sqlSchema string

// Link is the structure stored for each go short link.
type Link struct {
	Short     string // the "foo" part of http://go/foo
	Long      string // the target URL or text/template pattern to run
	Created   time.Time
	LastEdit  time.Time  // when the link was last edited (calculated from previous version)
	Owner     string     // user@domain
	DeletedAt *time.Time `json:",omitempty"` // when link was deleted (nil = not deleted)
	DeletedBy *string    `json:",omitempty"` // who deleted the link (nil = not deleted)
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
	mu sync.RWMutex

	clock tstime.Clock // allow overriding time for tests
}

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

	if err := migrateSchema(db); err != nil {
		return nil, err
	}

	return &SQLiteDB{db: db, clock: tstime.StdClock{}}, nil
}

// migrateSchema applies any necessary schema migrations to existing databases.
// When adding new columns to schema.sql, also add a migration here.
func migrateSchema(db *sql.DB) error {
	// Get actual columns from database
	rows, err := db.Query("PRAGMA table_info(Links)")
	if err != nil {
		return err
	}
	defer rows.Close()

	actualColumns := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name string
		var type_ string
		var notnull int
		var dfltValue *string
		var pk int

		if err := rows.Scan(&cid, &name, &type_, &notnull, &dfltValue, &pk); err != nil {
			return err
		}
		actualColumns[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Add DeletedAt column if missing (introduced for soft-delete feature)
	if !actualColumns["DeletedAt"] {
		if _, err := db.Exec("ALTER TABLE Links ADD COLUMN DeletedAt INTEGER DEFAULT NULL"); err != nil {
			return err
		}
	}

	// Add DeletedBy column if missing (introduced for soft-delete feature with audit trail)
	if !actualColumns["DeletedBy"] {
		if _, err := db.Exec("ALTER TABLE Links ADD COLUMN DeletedBy TEXT DEFAULT NULL"); err != nil {
			return err
		}
	}

	return nil
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
	rows, err := s.db.Query("SELECT Short, Long, Created, Owner, DeletedAt FROM Links WHERE DeletedAt IS NULL")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		link := new(Link)
		var created int64
		var deletedAt *int64
		err := rows.Scan(&link.Short, &link.Long, &created, &link.Owner, &deletedAt)
		if err != nil {
			return nil, err
		}
		link.Created = time.Unix(created, 0).UTC()
		link.LastEdit = s.getLastEditTime(linkID(link.Short))
		if deletedAt != nil {
			t := time.Unix(*deletedAt, 0).UTC()
			link.DeletedAt = &t
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

// LoadAllIncludingDeleted returns all stored Links, including soft-deleted ones.
//
// The caller owns the returned values.
func (s *SQLiteDB) LoadAllIncludingDeleted() ([]*Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var links []*Link
	rows, err := s.db.Query("SELECT Short, Long, Created, Owner, DeletedAt FROM Links")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		link := new(Link)
		var created int64
		var deletedAt *int64
		err := rows.Scan(&link.Short, &link.Long, &created, &link.Owner, &deletedAt)
		if err != nil {
			return nil, err
		}
		link.Created = time.Unix(created, 0).UTC()
		link.LastEdit = s.getLastEditTime(linkID(link.Short))
		if deletedAt != nil {
			t := time.Unix(*deletedAt, 0).UTC()
			link.DeletedAt = &t
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

// GetLinkHistory returns all versions of a link, including the active one and all soft-deleted ones.
// The versions are ordered by creation date, with the most recent version first.
func (s *SQLiteDB) GetLinkHistory(short string) ([]*Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var links []*Link
	rows, err := s.db.Query("SELECT Short, Long, Created, Owner, DeletedAt FROM Links WHERE ID = ? ORDER BY Created DESC", linkID(short))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		link := new(Link)
		var created int64
		var deletedAt *int64
		err := rows.Scan(&link.Short, &link.Long, &created, &link.Owner, &deletedAt)
		if err != nil {
			return nil, err
		}
		link.Created = time.Unix(created, 0).UTC()
		link.LastEdit = s.getLastEditTime(linkID(link.Short))
		if deletedAt != nil {
			t := time.Unix(*deletedAt, 0).UTC()
			link.DeletedAt = &t
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

// getLastEditTime returns the Created time of the most recent previous version (if any).
// This represents when the link was last edited by a user via SaveWithHistory.
// Note: This only tracks user edits, not deletions or other state changes.
// Called with the read lock held.
func (s *SQLiteDB) getLastEditTime(id string) time.Time {
	var lastEditUnix int64
	// Query the most recent soft-deleted version's Created time.
	// During SaveWithHistory, the previous active version is soft-deleted (marked with DeletedAt).
	// Its Created timestamp is the timestamp of that edit.
	row := s.db.QueryRow("SELECT Created FROM Links WHERE ID = ? AND DeletedAt IS NOT NULL ORDER BY Created DESC LIMIT 1", id)
	if err := row.Scan(&lastEditUnix); err != nil {
		// No previous version found (first version or link was hard-deleted), return zero time
		return time.Time{}
	}
	return time.Unix(lastEditUnix, 0).UTC()
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
	var created int64
	var deletedAt *int64
	id := linkID(short)
	row := s.db.QueryRow("SELECT Short, Long, Created, Owner, DeletedAt FROM Links WHERE ID = ?1 AND DeletedAt IS NULL LIMIT 1", id)
	err := row.Scan(&link.Short, &link.Long, &created, &link.Owner, &deletedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = fs.ErrNotExist
		}
		return nil, err
	}
	link.Created = time.Unix(created, 0).UTC()
	link.LastEdit = s.getLastEditTime(id)
	if deletedAt != nil {
		t := time.Unix(*deletedAt, 0).UTC()
		link.DeletedAt = &t
	}
	return link, nil
}

// Save saves a Link (idempotent - for internal use, imports, tests).
func (s *SQLiteDB) Save(link *Link) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var deletedAt *int64
	if link.DeletedAt != nil {
		t := link.DeletedAt.Unix()
		deletedAt = &t
	}

	// If Created is zero, set it to now. This is important for the UNIQUE(ID, Created) constraint.
	if link.Created.IsZero() {
		link.Created = s.Now().UTC()
	}

	result, err := s.db.Exec("INSERT OR REPLACE INTO Links (ID, Short, Long, Created, Owner, DeletedAt) VALUES (?, ?, ?, ?, ?, ?)", linkID(link.Short), link.Short, link.Long, link.Created.Unix(), link.Owner, deletedAt)
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

// SaveWithHistory saves a Link and preserves version history (for user edits).
// It soft-deletes the previous active version to keep it as historical record.
// Version history is maintained by:
//   - The current active version has Created=now, DeletedAt=NULL
//   - All previous versions have DeletedAt set to when they were superseded
//   - GetLinkHistory returns all versions ordered by Created DESC (newest first)
//   - getLastEditTime extracts the Created time from the most recent soft-deleted version
func (s *SQLiteDB) SaveWithHistory(ctx context.Context, link *Link) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var deletedAt *int64
	if link.DeletedAt != nil {
		t := link.DeletedAt.Unix()
		deletedAt = &t
	}

	id := linkID(link.Short)
	now := s.Now().Unix()

	// For SaveWithHistory, always use 'now' as the Created timestamp for the new version.
	// This ensures version history can be stored (each version needs a unique Created time).
	// The original creation time is preserved in previous versions.
	created := now

	// Soft-delete any existing active version (preserves history) and insert new version atomically
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Soft-delete any existing active version (preserves as history)
	updateResult, err := tx.Exec("UPDATE Links SET DeletedAt = ? WHERE ID = ? AND DeletedAt IS NULL", now, id)
	if err != nil {
		return err
	}
	updatedRows, err := updateResult.RowsAffected()
	if err != nil {
		return err
	}

	// If there was an active version, we've soft-deleted it. Now we need to insert a new row.
	// If there was no active version, we can still insert a new row (it's a fresh link or we're
	// restoring a deleted link).

	// Insert new version
	result, err := tx.Exec("INSERT INTO Links (ID, Short, Long, Created, Owner, DeletedAt) VALUES (?, ?, ?, ?, ?, ?)",
		id, link.Short, link.Long, created, link.Owner, deletedAt)
	if err != nil {
		// If we get a UNIQUE constraint error and we didn't update anything, it means the old row
		// is still there (maybe it was already deleted or this is a fresh link). Try INSERT OR REPLACE.
		if updatedRows == 0 && strings.Contains(err.Error(), "UNIQUE constraint failed") {
			result, err = tx.Exec("INSERT OR REPLACE INTO Links (ID, Short, Long, Created, Owner, DeletedAt) VALUES (?, ?, ?, ?, ?, ?)",
				id, link.Short, link.Long, created, link.Owner, deletedAt)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows < 1 {
		return fmt.Errorf("expected to affect at least 1 row, affected %d", rows)
	}

	return tx.Commit()
}

// Delete soft-deletes a Link using its short name (delayed deletion).
// The deletedBy user is retrieved from the context (set by setUserInContext middleware).
func (s *SQLiteDB) Delete(ctx context.Context, short string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	deletedByStr := getUserFromContext(ctx)
	var deletedBy *string
	if deletedByStr != "" {
		deletedBy = &deletedByStr
	}

	now := s.Now().Unix()
	result, err := s.db.ExecContext(ctx, "UPDATE Links SET DeletedAt = ?, DeletedBy = ? WHERE ID = ? AND DeletedAt IS NULL", now, deletedBy, linkID(short))
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

func (s *SQLiteDB) LoadDeleted(short string) (*Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	link := new(Link)
	var created int64
	var deletedAt *int64
	id := linkID(short)
	row := s.db.QueryRow("SELECT Short, Long, Created, Owner, DeletedAt, DeletedBy FROM Links WHERE ID = ?1 AND DeletedAt IS NOT NULL ORDER BY Created DESC LIMIT 1", id)
	err := row.Scan(&link.Short, &link.Long, &created, &link.Owner, &deletedAt, &link.DeletedBy)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = fs.ErrNotExist
		}
		return nil, err
	}
	link.Created = time.Unix(created, 0).UTC()
	link.LastEdit = s.getLastEditTime(id)
	if deletedAt != nil {
		t := time.Unix(*deletedAt, 0).UTC()
		link.DeletedAt = &t
	}
	return link, nil
}

// Undelete restores a soft-deleted Link.
func (s *SQLiteDB) Undelete(ctx context.Context, short string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Find the most recent deleted version for this ID
	var latestDeletedCreated int64
	row := s.db.QueryRowContext(ctx, "SELECT Created FROM Links WHERE ID = ? AND DeletedAt IS NOT NULL ORDER BY Created DESC LIMIT 1", linkID(short))
	if err := row.Scan(&latestDeletedCreated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no deleted link found for %q", short)
		}
		return err
	}

	result, err := s.db.ExecContext(ctx, "UPDATE Links SET DeletedAt = NULL, DeletedBy = NULL WHERE ID = ? AND Created = ?", linkID(short), latestDeletedCreated)
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

// CleanupDeleted permanently removes a batch of old deleted links.
// It preserves the most recent deleted record for each link ID for audit purposes.
// It returns the number of rows deleted.
func (s *SQLiteDB) CleanupDeleted(cutoff time.Time, batchSize int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec(`
		DELETE FROM Links
		WHERE rowid IN (
		  SELECT rowid FROM Links
		  WHERE DeletedAt IS NOT NULL
			AND DeletedAt < ?
			AND rowid NOT IN (
			  SELECT MAX(rowid)
			  FROM Links
			  WHERE DeletedAt IS NOT NULL
			  GROUP BY ID
			)
		  LIMIT ?
		)`, cutoff.Unix(), batchSize)
	if err != nil {
		return 0, err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}

	return int(rows), nil
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
