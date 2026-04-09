// Copyright 2022 Tailscale Inc & Contributors
// SPDX-License-Identifier: BSD-3-Clause

package golink

import (
	"path"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"tailscale.com/tstest"
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

// Test LoadStatsSince returns only stats since a given time.
func Test_SQLiteDB_LoadStatsSince(t *testing.T) {
	clock := tstest.NewClock(tstest.ClockOpts{
		Start: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	})

	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.clock = clock

	// preload links
	for _, link := range []*Link{{Short: "a"}, {Short: "b"}} {
		if err := db.Save(link); err != nil {
			t.Fatal(err)
		}
	}

	// save stats at initial time (old stats)
	if err := db.SaveStats(ClickStats{"a": 5, "b": 3}); err != nil {
		t.Fatal(err)
	}

	// advance 20 days and save more stats (recent stats)
	clock.Advance(20 * 24 * time.Hour)
	if err := db.SaveStats(ClickStats{"a": 2, "b": 7}); err != nil {
		t.Fatal(err)
	}

	// LoadStatsSince 10 days ago should only return the recent stats
	since := clock.Now().Add(-10 * 24 * time.Hour)
	got, err := db.LoadStatsSince(since)
	if err != nil {
		t.Fatal(err)
	}
	want := ClickStats{"a": 2, "b": 7}
	if !cmp.Equal(got, want) {
		t.Errorf("LoadStatsSince got %v, want %v", got, want)
	}

	// LoadStatsSince 30 days ago should return all stats
	since = clock.Now().Add(-30 * 24 * time.Hour)
	got, err = db.LoadStatsSince(since)
	if err != nil {
		t.Fatal(err)
	}
	want = ClickStats{"a": 7, "b": 10}
	if !cmp.Equal(got, want) {
		t.Errorf("LoadStatsSince got %v, want %v", got, want)
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
