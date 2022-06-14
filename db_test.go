package main

import (
	"path"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func Test_FileDB_linkPath(t *testing.T) {
	tests := []struct {
		short, want string
	}{
		{"foo", "foo"},
		{"FOO", "foo"},
		{"foo-bar", "foobar"},
		{"foo.bar", "foo.bar"},
		{"foo/bar", "foo%2Fbar"},
	}

	db := &FileDB{dir: "/tmp"}
	for _, tt := range tests {
		want := "/tmp/" + tt.want
		if got := db.linkPath(tt.short); got != want {
			t.Errorf("linkPath(%q) got %q, want %q", tt.short, got, want)
		}
	}
}

// Test saving and loading links for FileDB
func Test_FileDB_SaveLoadLinks(t *testing.T) {
	db, err := NewFileDB(t.TempDir(), false)
	if err != nil {
		t.Error(err)
	}

	testSaveAndLoadLinks(t, db)
}

// Test saving and loading stats for FileDB
func Test_FileDB_SaveLoadStats(t *testing.T) {
	db, err := NewFileDB(t.TempDir(), false)
	if err != nil {
		t.Error(err)
	}

	testSaveAndLoadStats(t, db)
}

// Test saving and loading links for SQLiteDB
func Test_SQLiteDB_SaveLoadLinks(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Error(err)
	}

	testSaveAndLoadLinks(t, db)
}

// Test saving and loading stats for SQLiteDB
func Test_SQLiteDB_SaveLoadStats(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Error(err)
	}

	testSaveAndLoadStats(t, db)
}

func testSaveAndLoadLinks(t *testing.T, db DB) {
	links := []*Link{
		{Short: "short", Long: "long"},
		{Short: "Foo.Bar", Long: "long"},
	}

	for _, link := range links {
		if err := db.Save(link); err != nil {
			t.Error(err)
		}
		got, err := db.Load(link.Short)
		if err != nil {
			t.Error(err)
		}

		if !cmp.Equal(got, link) {
			t.Errorf("db save and load got %v, want %v", *got, *link)
		}
	}

	got, err := db.LoadAll()
	if err != nil {
		t.Error(err)
	}

	sortLinks := cmpopts.SortSlices(func(a, b *Link) bool {
		return a.Short < b.Short
	})
	if !cmp.Equal(got, links, sortLinks) {
		t.Errorf("db.LoadAll got %v, want %v", got, links)
	}
}

func testSaveAndLoadStats(t *testing.T, db DB) {
	// preload some links
	links := []*Link{
		{Short: "a"},
		{Short: "b"},
	}
	for _, link := range links {
		if err := db.Save(link); err != nil {
			t.Error(err)
		}
	}

	// stats to record and then retrieve
	stats := []ClickStats{
		{"a": 1},
		{"b": 1},
		{"a": 1, "b": 2},
	}
	want := ClickStats{
		"a": 2,
		"b": 3,
	}

	for _, s := range stats {
		if err := db.SaveStats(s); err != nil {
			t.Error(err)
		}
	}

	got, err := db.LoadStats()
	if err != nil {
		t.Error(err)
	}
	if !cmp.Equal(got, want) {
		t.Errorf("db.LoadStats got %v, want %v", got, want)
	}
}
