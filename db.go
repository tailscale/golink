package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
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
	Clicks   int       `json:",omitempty"` // number of times this link has been served
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

// DB provides storage for Links.
type DB interface {
	// LoadAll returns all stored Links.
	//
	// The caller owns the returned values.
	LoadAll() ([]*Link, error)

	// Load returns a Link by its short name.
	//
	// It returns fs.ErrNotExist if the link does not exist.
	//
	// The caller owns the returned value.
	Load(short string) (*Link, error)

	// Save saves a Link.
	Save(*Link) error

	// LoadStats returns click stats for links.
	LoadStats() (ClickStats, error)

	// SaveStats records click stats for links.  The provided map includes
	// incremental clicks that have occurred since the last time SaveStats
	// was called.
	SaveStats(ClickStats) error
}

// FileDB stores Links in JSON files on disk.
type FileDB struct {
	// dir is the directory to store one JSON file per link.
	dir string
}

// NewFileDB returns a new FileDB which will store links in individual JSON
// files in the specified directory.  If mkdir is true, the directory will be
// created if it does not exist.
func NewFileDB(dir string, mkdir bool) (*FileDB, error) {
	if mkdir {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
	}
	if fi, err := os.Stat(dir); err != nil {
		return nil, err
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("%q is not a directory", dir)
	}
	return &FileDB{dir: dir}, nil
}

// linkPath returns the path to the file on disk for the specified link. Short
// name is normalized to be case insensitive, remove dashes, and escape some
// characters.
func (f *FileDB) linkPath(short string) string {
	name := linkID(short)
	name = strings.ReplaceAll(name, ".", "%2e")
	return filepath.Join(f.dir, name)
}

func (f *FileDB) LoadAll() ([]*Link, error) {
	d, err := os.Open(f.dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()

	names, err := d.Readdirnames(0)
	if err != nil {
		return nil, err
	}

	links := make([]*Link, len(names))
	for i, short := range names {
		link, err := f.Load(short)
		if err != nil {
			return nil, err
		}
		links[i] = link
	}

	return links, nil
}

func (f *FileDB) Load(short string) (*Link, error) {
	data, err := os.ReadFile(f.linkPath(short))
	if err != nil {
		return nil, err
	}
	link := new(Link)
	if err := json.Unmarshal(data, link); err != nil {
		return nil, err
	}
	return link, nil
}

func (f *FileDB) Save(link *Link) error {
	j, err := json.MarshalIndent(link, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(f.linkPath(link.Short), j, 0600); err != nil {
		return err
	}
	return nil
}

func (f *FileDB) LoadStats() (ClickStats, error) {
	links, err := db.LoadAll()
	if err != nil {
		return nil, err
	}

	stats := make(ClickStats)
	for _, link := range links {
		if link.Clicks > 0 {
			stats[link.Short] = link.Clicks
		}
	}

	return stats, nil
}

func (f *FileDB) SaveStats(stats ClickStats) error {
	for short, clicks := range stats {
		if clicks <= 0 {
			continue
		}
		link, err := f.Load(short)
		if err != nil {
			return err
		}
		link.Clicks += clicks
		if err := f.Save(link); err != nil {
			return err
		}
	}
	return nil
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

func (s *SQLiteDB) LoadStats() (ClickStats, error) {
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
		stats[id] = clicks
	}
	return stats, rows.Err()
}

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
