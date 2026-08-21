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
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	"tailscale.com/tstime"
)

//go:embed schema.sql
var sqlSchema string

// ErrActiveLinkExists is returned by Undelete when an active link already
// exists at the requested short name.
var ErrActiveLinkExists = errors.New("an active link already exists at this name")

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

// Close closes the underlying database connection.
func (s *SQLiteDB) Close() error {
	return s.db.Close()
}

// migrateSchema applies any necessary schema migrations to existing databases.
// When adding new columns to schema.sql, also add a migration here.
func migrateSchema(db *sql.DB) error {
	actualColumns, idIsPrimaryKey, err := linksTableColumns(db)
	if err != nil {
		return err
	}

	// Versions of this schema before soft-deletion used ID as a PRIMARY KEY,
	// which only allows one row per link. Version history and soft-deletion
	// both require storing multiple rows per ID (distinguished by Created),
	// so the old PRIMARY KEY must be dropped. SQLite has no ALTER TABLE
	// support for dropping a PRIMARY KEY, so the table is rebuilt instead.
	if idIsPrimaryKey {
		if err := rebuildLinksTableWithoutPrimaryKey(db, actualColumns["DeletedAt"], actualColumns["DeletedBy"]); err != nil {
			return fmt.Errorf("migrating away from PRIMARY KEY(ID): %w", err)
		}
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

// linksTableColumns returns the set of column names present in the Links
// table, and whether ID is (still) declared as a PRIMARY KEY.
func linksTableColumns(db *sql.DB) (columns map[string]bool, idIsPrimaryKey bool, err error) {
	rows, err := db.Query("PRAGMA table_info(Links)")
	if err != nil {
		return nil, false, err
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil {
			log.Printf("failed to close rows: %v", cerr)
		}
	}()

	columns = make(map[string]bool)
	for rows.Next() {
		var cid int
		var name string
		var type_ string
		var notnull int
		var dfltValue *string
		var pk int

		if err := rows.Scan(&cid, &name, &type_, &notnull, &dfltValue, &pk); err != nil {
			return nil, false, err
		}
		columns[name] = true
		if name == "ID" && pk > 0 {
			idIsPrimaryKey = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return columns, idIsPrimaryKey, nil
}

// rebuildLinksTableWithoutPrimaryKey replaces a Links table that still has
// the old `ID TEXT PRIMARY KEY` constraint with one using
// `UNIQUE(ID, Created)`, preserving all existing rows. hasDeletedAt and
// hasDeletedBy indicate whether the old table already had those columns
// (e.g. a database that picked them up via the ADD COLUMN migration below
// on a prior run, but predates the UNIQUE(ID, Created) change), so their
// data is carried through the rebuild rather than silently discarded.
func rebuildLinksTableWithoutPrimaryKey(db *sql.DB, hasDeletedAt, hasDeletedBy bool) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			log.Printf("failed to rollback transaction: %v", err)
		}
	}()

	if _, err := tx.Exec(`ALTER TABLE Links RENAME TO Links_pre_history_migration`); err != nil {
		return err
	}
	if _, err := tx.Exec(`
CREATE TABLE Links (
    ID      TEXT    NOT NULL,
    Short   TEXT    NOT NULL DEFAULT "",
    Long    TEXT    NOT NULL DEFAULT "",
    Created INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
    Owner   TEXT    NOT NULL DEFAULT "",
    UNIQUE (ID, Created)
)`); err != nil {
		return err
	}

	columns := []string{"ID", "Short", "Long", "Created", "Owner"}
	if hasDeletedAt {
		if _, err := tx.Exec(`ALTER TABLE Links ADD COLUMN DeletedAt INTEGER DEFAULT NULL`); err != nil {
			return err
		}
		columns = append(columns, "DeletedAt")
	}
	if hasDeletedBy {
		if _, err := tx.Exec(`ALTER TABLE Links ADD COLUMN DeletedBy TEXT DEFAULT NULL`); err != nil {
			return err
		}
		columns = append(columns, "DeletedBy")
	}

	colList := strings.Join(columns, ", ")
	if _, err := tx.Exec(fmt.Sprintf(`INSERT INTO Links (%s) SELECT %s FROM Links_pre_history_migration`, colList, colList)); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE Links_pre_history_migration`); err != nil {
		return err
	}
	return tx.Commit()
}

// Now returns the current time.
func (s *SQLiteDB) Now() time.Time {
	return tstime.DefaultClock{Clock: s.clock}.Now()
}

// lastEditColumn is a correlated subquery computing, for each row, the
// Created time of the version it superseded: the most recent soft-deleted
// row for the same ID with an earlier Created time. It's NULL for a row
// that is itself the first version. Folding this into the main query (as
// opposed to a separate per-row lookup) means every row gets its own
// correct value, and listing N links costs one query instead of N+1.
const lastEditColumn = `(SELECT MAX(prev.Created) FROM Links prev WHERE prev.ID = Links.ID AND prev.DeletedAt IS NOT NULL AND prev.Created < Links.Created)`

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanLink scans a row selected as "Short, Long, Created, Owner, DeletedAt, <lastEditColumn>".
func scanLink(rs rowScanner) (*Link, error) {
	link := new(Link)
	var created int64
	var deletedAt, lastEdit *int64
	if err := rs.Scan(&link.Short, &link.Long, &created, &link.Owner, &deletedAt, &lastEdit); err != nil {
		return nil, err
	}
	link.Created = time.Unix(created, 0).UTC()
	if deletedAt != nil {
		t := time.Unix(*deletedAt, 0).UTC()
		link.DeletedAt = &t
	}
	if lastEdit != nil {
		link.LastEdit = time.Unix(*lastEdit, 0).UTC()
	}
	return link, nil
}

// LoadAll returns all stored Links.
//
// The caller owns the returned values.
func (s *SQLiteDB) LoadAll() ([]*Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var links []*Link
	rows, err := s.db.Query("SELECT Short, Long, Created, Owner, DeletedAt, " + lastEditColumn + " FROM Links WHERE DeletedAt IS NULL")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		link, err := scanLink(rows)
		if err != nil {
			return nil, err
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
	rows, err := s.db.Query("SELECT Short, Long, Created, Owner, DeletedAt, " + lastEditColumn + " FROM Links")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		link, err := scanLink(rows)
		if err != nil {
			return nil, err
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
	rows, err := s.db.Query("SELECT Short, Long, Created, Owner, DeletedAt, "+lastEditColumn+" FROM Links WHERE ID = ? ORDER BY Created DESC", linkID(short))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		link, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
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

	row := s.db.QueryRow("SELECT Short, Long, Created, Owner, DeletedAt, "+lastEditColumn+" FROM Links WHERE ID = ?1 AND DeletedAt IS NULL LIMIT 1", linkID(short))
	link, err := scanLink(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fs.ErrNotExist
		}
		return nil, err
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
func (s *SQLiteDB) SaveWithHistory(ctx context.Context, link *Link) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var deletedAt *int64
	if link.DeletedAt != nil {
		t := link.DeletedAt.Unix()
		deletedAt = &t
	}

	id := linkID(link.Short)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			log.Printf("failed to rollback transaction: %v", err)
		}
	}()

	// Created has only second resolution, so consecutive edits within the
	// same second would otherwise collide with the just-superseded row on
	// UNIQUE(ID, Created). Anchor the new version strictly after every
	// existing version (active or historical) of this link instead.
	var maxCreated int64
	if err := tx.QueryRow("SELECT COALESCE(MAX(Created), 0) FROM Links WHERE ID = ?", id).Scan(&maxCreated); err != nil {
		return err
	}
	created := s.Now().Unix()
	if created <= maxCreated {
		created = maxCreated + 1
	}

	// Soft-delete any existing active version (preserves as history). If
	// there was no active version, this affects 0 rows (fresh link, or
	// recreating one whose active version was already deleted).
	if _, err := tx.Exec("UPDATE Links SET DeletedAt = ? WHERE ID = ? AND DeletedAt IS NULL", created, id); err != nil {
		return err
	}

	result, err := tx.Exec("INSERT INTO Links (ID, Short, Long, Created, Owner, DeletedAt) VALUES (?, ?, ?, ?, ?, ?)",
		id, link.Short, link.Long, created, link.Owner, deletedAt)
	if err != nil {
		return err
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
	var deletedAt, lastEdit *int64
	id := linkID(short)
	row := s.db.QueryRow("SELECT Short, Long, Created, Owner, DeletedAt, DeletedBy, "+lastEditColumn+" FROM Links WHERE ID = ?1 AND DeletedAt IS NOT NULL ORDER BY Created DESC LIMIT 1", id)
	err := row.Scan(&link.Short, &link.Long, &created, &link.Owner, &deletedAt, &link.DeletedBy, &lastEdit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = fs.ErrNotExist
		}
		return nil, err
	}
	link.Created = time.Unix(created, 0).UTC()
	if lastEdit != nil {
		t := time.Unix(*lastEdit, 0).UTC()
		link.LastEdit = t
	}
	if deletedAt != nil {
		t := time.Unix(*deletedAt, 0).UTC()
		link.DeletedAt = &t
	}
	return link, nil
}

// Undelete restores a soft-deleted Link.
// It returns an error if an active link already exists at short (e.g. someone
// recreated the name while the original was pending deletion), since restoring
// would otherwise leave two active versions of the same link.
func (s *SQLiteDB) Undelete(ctx context.Context, short string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := linkID(short)

	var activeCount int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM Links WHERE ID = ? AND DeletedAt IS NULL", id).Scan(&activeCount); err != nil {
		return err
	}
	if activeCount > 0 {
		return fmt.Errorf("cannot undelete %q: %w", short, ErrActiveLinkExists)
	}

	// Find the most recent deleted version for this ID
	var latestDeletedCreated int64
	row := s.db.QueryRowContext(ctx, "SELECT Created FROM Links WHERE ID = ? AND DeletedAt IS NOT NULL ORDER BY Created DESC LIMIT 1", id)
	if err := row.Scan(&latestDeletedCreated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no deleted link found for %q", short)
		}
		return err
	}

	result, err := s.db.ExecContext(ctx, "UPDATE Links SET DeletedAt = NULL, DeletedBy = NULL WHERE ID = ? AND Created = ?", id, latestDeletedCreated)
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
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				log.Printf("failed to rollback transaction: %v", rollbackErr)
			}
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
	rows, err := s.db.Query("SELECT Short, Long, Created, Owner, DeletedAt, "+lastEditColumn+" FROM Links WHERE DeletedAt IS NULL AND LOWER(Owner) = LOWER(?)", owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		link, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}
