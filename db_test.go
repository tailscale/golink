// Copyright 2022 Tailscale Inc & Contributors
// SPDX-License-Identifier: BSD-3-Clause

package golink

import (
	"path"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// Test saving, loading, and deleting links for SQLiteDB.
func Test_SQLiteDB_SaveLoadDeleteLinks(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Error(err)
	}

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

	for _, link := range links {
		if err := db.Delete(link.Short); err != nil {
			t.Error(err)
		}
	}

	got, err = db.LoadAll()
	if err != nil {
		t.Error(err)
	}
	want := []*Link(nil)
	if !cmp.Equal(got, want) {
		t.Errorf("db.LoadAll got %v, want %v", got, want)
	}
}

// Test saving, loading, and deleting stats for SQLiteDB.
func Test_SQLiteDB_SaveLoadDeleteStats(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Error(err)
	}

	// preload some links
	links := []*Link{
		{Short: "a"},
		{Short: "B-c"},
	}
	for _, link := range links {
		if err := db.Save(link); err != nil {
			t.Error(err)
		}
	}

	// Stats to record and then retrieve.
	// Stats to store do not need to be their canonical short name,
	// but returned stats always should be.
	stats := []ClickStats{
		{"a": 1},
		{"b-c": 1},
		{"a": 1, "bc": 2},
	}
	want := ClickStats{
		"a":   2,
		"B-c": 3,
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

	for k := range want {
		if err := db.DeleteStats(k); err != nil {
			t.Error(err)
		}
	}

	got, err = db.LoadStats()
	if err != nil {
		t.Error(err)
	}
	want = ClickStats{}
	if !cmp.Equal(got, want) {
		t.Errorf("db.LoadStats got %v, want %v", got, want)
	}
}

// Test GetLinksByOwner functionality
func Test_SQLiteDB_GetLinksByOwner(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Error(err)
	}

	// preload some links with owner
	links := []*Link{
		{Short: "a", Owner: "foo@bar.com"},
		{Short: "B-c", Owner: "bar@foo.com "},
	}
	for _, link := range links {
		if err := db.Save(link); err != nil {
			t.Error(err)
		}
	}

	want := []*Link{
		{Short: "a", Owner: "foo@bar.com"},
	}
	got, err := db.GetLinksByOwner("foo@bar.com")
	if err != nil {
		t.Error(err)
	}

	if !cmp.Equal(got, want) {
		t.Errorf("db.GetLinksByOwner got %v; want %v", got, want)
	}

	// confirm empty response for non-existant owner
	got, err = db.GetLinksByOwner("foo1@bar.com")
	if err != nil {
		t.Error(err)
	}
	if len(got) != 0 {
		t.Errorf("db.GetLinksByOwner got %v; want empty slice", got)
	}
}

// Test_SQLiteDB_IndexStaysInSync verifies that Save, SaveAll, and Delete
// keep the in-memory link index consistent with the SQL table.
func Test_SQLiteDB_IndexStaysInSync(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// snapshot compares the in-memory idx against a fresh LoadAll of
	// the SQLite table, keyed by linkID. Link values are compared by
	// Short/Long/Owner (timestamps are truncated to seconds by storage
	// and preserved verbatim in the index, so we ignore them here).
	snapshot := func(stepName string) {
		t.Helper()
		got, err := db.LoadAll()
		if err != nil {
			t.Fatalf("[%s] LoadAll: %v", stepName, err)
		}
		wantIDs := make(map[string]bool, len(got))
		for _, l := range got {
			wantIDs[linkID(l.Short)] = true
		}
		db.mu.RLock()
		defer db.mu.RUnlock()
		if len(db.idx) != len(got) {
			t.Errorf("[%s] idx size = %d, db rows = %d", stepName, len(db.idx), len(got))
		}
		for id := range wantIDs {
			l, ok := db.idx[id]
			if !ok {
				t.Errorf("[%s] row with id %q not present in idx", stepName, id)
				continue
			}
			if linkID(l.Short) != id {
				t.Errorf("[%s] idx key %q points to link with Short %q (expected linkID to match)", stepName, id, l.Short)
			}
		}
		for id, l := range db.idx {
			if !wantIDs[id] {
				t.Errorf("[%s] idx contains id %q (%+v) not in DB", stepName, id, l)
			}
		}
	}

	snapshot("empty")

	// Save one.
	if err := db.Save(&Link{Short: "foo", Long: "http://foo/"}); err != nil {
		t.Fatal(err)
	}
	snapshot("after save foo")

	// Overwrite it.
	if err := db.Save(&Link{Short: "foo", Long: "http://foo2/"}); err != nil {
		t.Fatal(err)
	}
	if l := db.idx[linkID("foo")]; l == nil || l.Long != "http://foo2/" {
		t.Errorf("idx foo.Long = %q, want http://foo2/", l.Long)
	}
	snapshot("after overwrite foo")

	// SaveAbsent with a mix of new and existing links.
	n, err := db.SaveAbsent([]*Link{
		{Short: "foo", Long: "http://ignored/"}, // INSERT OR IGNORE: should not overwrite
		{Short: "bar", Long: "http://bar/"},
		{Short: "baz", Long: "http://baz/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("SaveAll inserted = %d, want 2", n)
	}
	if l := db.idx[linkID("foo")]; l == nil || l.Long != "http://foo2/" {
		t.Errorf("after SaveAll, idx foo.Long = %q, want unchanged http://foo2/", l.Long)
	}
	snapshot("after SaveAll")

	// Delete one.
	if err := db.Delete("bar"); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.idx[linkID("bar")]; ok {
		t.Errorf("idx still contains bar after Delete")
	}
	snapshot("after delete bar")

	// Caller mutates the link struct after Save: the idx entry must
	// not change (we store a copy).
	orig := &Link{Short: "mut", Long: "http://before/"}
	if err := db.Save(orig); err != nil {
		t.Fatal(err)
	}
	orig.Long = "http://after/"
	if l := db.idx[linkID("mut")]; l == nil || l.Long != "http://before/" {
		t.Errorf("idx mut.Long = %v, want http://before/ (caller-side mutation leaked into idx)", l)
	}
}
