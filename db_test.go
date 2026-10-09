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

// Test_SQLiteDB_SearchShort verifies the fuzzy autocomplete ranking.
func Test_SQLiteDB_SearchShort(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}
	corpus := []*Link{
		{Short: "foo", Long: "http://foo/"},
		{Short: "foo-bar", Long: "http://foobar/"},
		{Short: "food", Long: "http://food/"},
		{Short: "2023-review", Long: "http://docs/2023"},
		{Short: "team.docs", Long: "http://docs/team"},
		{Short: "unrelated", Long: "http://example/"},
	}
	for _, l := range corpus {
		if err := db.Save(l); err != nil {
			t.Fatal(err)
		}
	}

	// Empty query yields no matches.
	if got := db.SearchShort("", false, searchFilters{}, 10); len(got) != 0 {
		t.Errorf("SearchShort empty query got %d results, want 0", len(got))
	}

	// Exact prefix "foo" should rank "foo" first (first-char bonus + length).
	got := db.SearchShort("foo", false, searchFilters{}, 10)
	if len(got) == 0 || got[0].Link.Short != "foo" {
		t.Errorf("SearchShort(foo) top = %v, want foo first; got %d results", firstShort(got), len(got))
	}

	// "fb" should prefer "foo-bar" (separator-boundary bonus on 'b'
	// after '-') over other candidates that contain 'f' and 'b'.
	got = db.SearchShort("fb", false, searchFilters{}, 10)
	if len(got) == 0 || got[0].Link.Short != "foo-bar" {
		t.Errorf("SearchShort(fb) top = %v, want foo-bar first", firstShort(got))
	}

	// Matched indexes should be returned and be valid rune offsets.
	got = db.SearchShort("fb", false, searchFilters{}, 10)
	if len(got) > 0 {
		idxs := got[0].ShortMatchedIndexes
		if len(idxs) != 2 {
			t.Errorf("expected 2 short matched indexes, got %v", idxs)
		}
		if got[0].LongMatchedIndexes != nil {
			t.Errorf("LongMatchedIndexes = %v, want nil (not includeLong)", got[0].LongMatchedIndexes)
		}
		if !got[0].MatchedShort {
			t.Errorf("MatchedShort = false, want true")
		}
	}

	// Limit respected.
	got = db.SearchShort("o", false, searchFilters{}, 2)
	if len(got) > 2 {
		t.Errorf("SearchShort(o, limit=2) returned %d results, want <=2", len(got))
	}

	// includeLong mode: query that only appears in the Long field
	// should produce a match.
	got = db.SearchShort("example", true, searchFilters{}, 10)
	foundUnrelated := false
	for _, m := range got {
		if m.Link.Short == "unrelated" {
			foundUnrelated = true
			break
		}
	}
	if !foundUnrelated {
		t.Errorf("SearchShort(example, includeLong=true) did not return unrelated (which matches via Long): %v", allShorts(got))
	}
}

func firstShort(ms []SearchMatch) string {
	if len(ms) == 0 {
		return "<none>"
	}
	return ms[0].Link.Short
}

func allShorts(ms []SearchMatch) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Link.Short)
	}
	return out
}

// Test_SQLiteDB_SearchShort_OwnerFilter verifies the owner filter, including
// case-insensitive matching and the filter-only listing (empty query).
func Test_SQLiteDB_SearchShort_OwnerFilter(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}
	corpus := []*Link{
		{Short: "alpha", Owner: "foo@bar.com"},
		{Short: "alias", Owner: "foo@bar.com"},
		{Short: "beta", Owner: "FOO@BAR.COM"},
		{Short: "gamma", Owner: "other@bar.com"},
	}
	for _, l := range corpus {
		if err := db.Save(l); err != nil {
			t.Fatal(err)
		}
	}

	// Filter-only (empty query) lists every link owned by the address,
	// case-insensitively, sorted by short name.
	got := db.SearchShort("", false, searchFilters{Owner: "foo@bar.com"}, 10)
	if want := []string{"alias", "alpha", "beta"}; !cmp.Equal(allShorts(got), want) {
		t.Errorf("owner filter got %v; want %v", allShorts(got), want)
	}

	// An owner with no links yields nothing.
	if got := db.SearchShort("", false, searchFilters{Owner: "nobody@bar.com"}, 10); len(got) != 0 {
		t.Errorf("unknown owner got %d results, want 0", len(got))
	}

	// Filter + fuzzy text ranks within the owner's links only; "gamma"
	// (a different owner) must not appear even though it fuzzy-matches "a".
	got = db.SearchShort("al", false, searchFilters{Owner: "foo@bar.com"}, 10)
	for _, m := range got {
		if m.Link.Owner == "other@bar.com" {
			t.Errorf("owner filter leaked a link owned by %q", m.Link.Owner)
		}
	}
	if len(got) == 0 {
		t.Errorf("owner filter + query returned no results, want matches for al")
	}
}
