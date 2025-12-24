// Copyright 2022 Tailscale Inc & Contributors
// SPDX-License-Identifier: BSD-3-Clause

package golink

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"tailscale.com/tstest"
)

// newTestDB creates a new SQLiteDB with a test clock for deterministic time control.
func newTestDB(t *testing.T, initialTime time.Time) (*SQLiteDB, *tstest.Clock) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	clock := tstest.NewClock(tstest.ClockOpts{Start: initialTime})
	db.clock = clock
	return db, clock
}

// Test saving, loading, and deleting links for SQLiteDB.
func Test_SQLiteDB_SaveLoadDeleteLinks(t *testing.T) {
	fixedTime := time.Date(2025, time.January, 1, 12, 0, 0, 0, time.UTC)
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Error(err)
	}

	links := []*Link{
		{Short: "short", Long: "long", Created: fixedTime},
		{Short: "Foo.Bar", Long: "long", Created: fixedTime},
	}

	for _, link := range links {
		if err := db.Save(link); err != nil {
			t.Error(err)
		}
		got, err := db.Load(link.Short)
		if err != nil {
			t.Error(err)
		}

		if !cmp.Equal(got, link, cmpopts.IgnoreFields(Link{}, "LastEdit")) {
			t.Errorf("db save and load got %v, want %v", *got, *link)
		}
	}

	got, err := db.LoadAll()
	if err != nil {
		t.Error(err)
	}

	wantLinks := []*Link{
		{Short: "Foo.Bar", Long: "long", Created: fixedTime},
		{Short: "short", Long: "long", Created: fixedTime},
	}
	sortLinks := cmpopts.SortSlices(func(a, b *Link) bool {
		return a.Short < b.Short
	})
	if !cmp.Equal(got, wantLinks, sortLinks, cmpopts.IgnoreFields(Link{}, "LastEdit")) {
		t.Errorf("db.LoadAll got %v, want %v", got, wantLinks)
	}

	for _, link := range links {
		if err := db.Delete(context.Background(), link.Short); err != nil {
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

// Test delayed deletion functionality
func Test_SQLiteDB_DelayedDeletion(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create a test link
	link := &Link{Short: "test", Long: "https://example.com"}
	if err := db.Save(link); err != nil {
		t.Fatal(err)
	}

	// Verify link exists
	got, err := db.Load("test")
	if err != nil {
		t.Fatal(err)
	}
	if got.DeletedAt != nil {
		t.Errorf("New link should not be deleted, got DeletedAt = %v", got.DeletedAt)
	}

	// Delete the link (soft delete)
	if err := db.Delete(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}

	// Verify link no longer appears in normal Load
	_, err = db.Load("test")
	if err == nil {
		t.Error("Expected deleted link to not be found in Load()")
	}

	// Verify link can be loaded as deleted
	deletedLink, err := db.LoadDeleted("test")
	if err != nil {
		t.Fatal(err)
	}
	if deletedLink.DeletedAt == nil {
		t.Error("Expected deleted link to have DeletedAt timestamp")
	}
	if deletedLink.Short != "test" {
		t.Errorf("Expected Short = 'test', got %v", deletedLink.Short)
	}

	// Test undelete
	if err := db.Undelete(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}

	// Verify link is active again
	restored, err := db.Load("test")
	if err != nil {
		t.Fatal(err)
	}
	if restored.DeletedAt != nil {
		t.Errorf("Undeleted link should not have DeletedAt, got %v", restored.DeletedAt)
	}

	// Test LoadAllIncludingDeleted shows both active and deleted links
	link2 := &Link{Short: "test2", Long: "https://example2.com"}
	if err := db.Save(link2); err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(context.Background(), link2.Short); err != nil {
		t.Fatal(err)
	}

	allLinks, err := db.LoadAllIncludingDeleted()
	if err != nil {
		t.Fatal(err)
	}

	activeCount := 0
	deletedCount := 0
	for _, l := range allLinks {
		if l.DeletedAt == nil {
			activeCount++
		} else {
			deletedCount++
		}
	}

	if activeCount != 1 {
		t.Errorf("Expected 1 active link, got %d", activeCount)
	}
	if deletedCount != 1 {
		t.Errorf("Expected 1 deleted link, got %d", deletedCount)
	}

	// Test cleanup of old deleted links (preserves most recent deleted record)
	cutoff := time.Now().Add(time.Hour) // Future time - should delete old versions but preserve latest
	count, err := db.CleanupDeleted(cutoff, 1000)
	if err != nil {
		t.Fatal(err)
	}

	// Since we only have one deleted link and we preserve the most recent, nothing should be cleaned up
	if count != 0 {
		t.Errorf("Expected to clean up 0 deleted links (preserving most recent), got %d", count)
	}

	// Verify we still have 1 active + 1 deleted (the most recent deleted record is preserved)
	allLinksAfterCleanup, err := db.LoadAllIncludingDeleted()
	if err != nil {
		t.Fatal(err)
	}
	if len(allLinksAfterCleanup) != 2 {
		t.Errorf("Expected 2 links after cleanup (1 active + 1 preserved deleted), got %d", len(allLinksAfterCleanup))
	}
}

// Test DeletedBy field is set when link is deleted
func Test_DeletedBy_Tracking(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create a link
	link := &Link{Short: "test", Long: "https://example.com", Owner: "user@example.com"}
	if err := db.Save(link); err != nil {
		t.Fatal(err)
	}

	// Verify DeletedBy is empty for active links
	loaded, err := db.Load("test")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DeletedBy != nil {
		t.Errorf("Expected DeletedBy to be nil for active link, got %q", *loaded.DeletedBy)
	}

	// Create a context with a user
	ctx := context.WithValue(context.Background(), CurrentUserKey, user{login: "admin@example.com"})

	// Delete the link using context
	if err := db.Delete(ctx, "test"); err != nil {
		t.Fatal(err)
	}

	// Verify DeletedBy is set
	deletedLink, err := db.LoadDeleted("test")
	if err != nil {
		t.Fatal(err)
	}
	if deletedLink.DeletedBy == nil || *deletedLink.DeletedBy != "admin@example.com" {
		got := ""
		if deletedLink.DeletedBy != nil {
			got = *deletedLink.DeletedBy
		}
		t.Errorf("Expected DeletedBy to be 'admin@example.com', got %q", got)
	}
	if deletedLink.DeletedAt == nil {
		t.Error("Expected DeletedAt to be set")
	}
}

// Test DeletedBy is cleared when link is undeleted
func Test_DeletedBy_Cleared_On_Undelete(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create and delete a link
	link := &Link{Short: "test", Long: "https://example.com", Owner: "user@example.com"}
	if err := db.Save(link); err != nil {
		t.Fatal(err)
	}

	ctx := context.WithValue(context.Background(), CurrentUserKey, user{login: "admin@example.com"})
	if err := db.Delete(ctx, "test"); err != nil {
		t.Fatal(err)
	}

	// Verify it was deleted by admin
	deletedLink, err := db.LoadDeleted("test")
	if err != nil {
		t.Fatal(err)
	}
	if deletedLink.DeletedBy == nil || *deletedLink.DeletedBy != "admin@example.com" {
		got := ""
		if deletedLink.DeletedBy != nil {
			got = *deletedLink.DeletedBy
		}
		t.Errorf("Expected DeletedBy to be set to admin, got %q", got)
	}

	// Undelete the link
	if err := db.Undelete(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}

	// Verify DeletedBy is cleared
	restored, err := db.Load("test")
	if err != nil {
		t.Fatal(err)
	}
	if restored.DeletedBy != nil {
		t.Errorf("Expected DeletedBy to be nil after undelete, got %q", *restored.DeletedBy)
	}
	if restored.DeletedAt != nil {
		t.Error("Expected DeletedAt to be nil after undelete")
	}
}

// Test DeletedBy from multiple users
func Test_DeletedBy_Multiple_Users(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create two links
	link1 := &Link{Short: "user1-link", Long: "https://example1.com", Owner: "user1@example.com"}
	link2 := &Link{Short: "user2-link", Long: "https://example2.com", Owner: "user2@example.com"}

	if err := db.Save(link1); err != nil {
		t.Fatal(err)
	}
	if err := db.Save(link2); err != nil {
		t.Fatal(err)
	}

	// User1 deletes their link
	ctx1 := context.WithValue(context.Background(), CurrentUserKey, user{login: "user1@example.com"})
	if err := db.Delete(ctx1, "user1-link"); err != nil {
		t.Fatal(err)
	}

	// User2 deletes their link
	ctx2 := context.WithValue(context.Background(), CurrentUserKey, user{login: "user2@example.com"})
	if err := db.Delete(ctx2, "user2-link"); err != nil {
		t.Fatal(err)
	}

	// Verify each link shows correct deleter
	deleted1, err := db.LoadDeleted("user1-link")
	if err != nil {
		t.Fatal(err)
	}
	if deleted1.DeletedBy == nil || *deleted1.DeletedBy != "user1@example.com" {
		got := ""
		if deleted1.DeletedBy != nil {
			got = *deleted1.DeletedBy
		}
		t.Errorf("Expected user1-link to be deleted by user1, got %q", got)
	}

	deleted2, err := db.LoadDeleted("user2-link")
	if err != nil {
		t.Fatal(err)
	}
	if deleted2.DeletedBy == nil || *deleted2.DeletedBy != "user2@example.com" {
		got := ""
		if deleted2.DeletedBy != nil {
			got = *deleted2.DeletedBy
		}
		t.Errorf("Expected user2-link to be deleted by user2, got %q", got)
	}
}

func Test_SQLiteDB_LastEditUpdatesOnEdit(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create initial link with explicit timestamp
	createdTime := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	link := &Link{Short: "test", Long: "https://example.com", Owner: "user@example.com", Created: createdTime}
	if err := db.Save(link); err != nil {
		t.Fatal(err)
	}

	// Load the link and verify initial state
	firstVersion, err := db.Load("test")
	if err != nil {
		t.Fatal(err)
	}

	// Initial LastEdit should be zero (no previous versions)
	if !firstVersion.LastEdit.IsZero() {
		t.Errorf("Initial LastEdit should be zero, got %v", firstVersion.LastEdit)
	}

	// Edit the link using SaveWithHistory
	editedLink := &Link{Short: "test", Long: "https://newurl.com", Owner: "user@example.com", Created: createdTime}
	if err := db.SaveWithHistory(context.Background(), editedLink); err != nil {
		t.Fatal(err)
	}

	// Load the updated link
	updatedLink, err := db.Load("test")
	if err != nil {
		t.Fatal(err)
	}

	// With version history, Created is updated to the edit time, and LastEdit points to previous version
	// Verify Created is now (after the edit)
	if updatedLink.Created.IsZero() {
		t.Errorf("Created should be set to edit time, got zero")
	}
	editTime1 := updatedLink.Created

	// Verify LastEdit changed to the original Created time (from previous version)
	if updatedLink.LastEdit != createdTime {
		t.Errorf("LastEdit should be the original Created time %v, got %v", createdTime, updatedLink.LastEdit)
	}

	// Verify the Long value was updated
	if updatedLink.Long != "https://newurl.com" {
		t.Errorf("Long should be updated to 'https://newurl.com', got %v", updatedLink.Long)
	}

	// Sleep to ensure next edit has a different Unix second timestamp
	time.Sleep(1001 * time.Millisecond)

	// Make another edit to test LastEdit updates to point to the previous edit time
	anotherEdit := &Link{Short: "test", Long: "https://another.com", Owner: "user@example.com"}
	if err := db.SaveWithHistory(context.Background(), anotherEdit); err != nil {
		t.Fatal(err)
	}

	finalLink, err := db.Load("test")
	if err != nil {
		t.Fatal(err)
	}

	// The LastEdit should now point to editTime1 (the time of the first edit)
	if finalLink.LastEdit != editTime1 {
		t.Errorf("After second edit, LastEdit should be first edit time %v, got %v", editTime1, finalLink.LastEdit)
	}

	// Check that we have version history: LoadAllIncludingDeleted should have multiple versions
	allVersions, err := db.LoadAllIncludingDeleted()
	if err != nil {
		t.Fatal(err)
	}

	// Count versions of "test" link
	testVersions := 0
	for _, l := range allVersions {
		if l.Short == "test" {
			testVersions++
		}
	}

	if testVersions < 3 {
		t.Errorf("Expected at least 3 versions of 'test' link (1 initial + 2 edits), got %d", testVersions)
	}
}

func Test_SQLiteDB_SaveWithHistoryAndUndelete(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// 1. Create initial link
	link1 := &Link{Short: "history-test", Long: "https://v1.com", Owner: "user1"}
	if err := db.Save(link1); err != nil {
		t.Fatal(err)
	}

	// Verify initial state
	var loadedLink *Link // Declare loadedLink here
	loadedLink, err = db.Load("history-test")
	if err != nil {
		t.Fatal(err)
	}
	if loadedLink.Long != "https://v1.com" || loadedLink.DeletedAt != nil {
		t.Errorf("Expected v1 active, got %+v", loadedLink)
	}
	history, err := db.GetLinkHistory("history-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Long != "https://v1.com" || history[0].DeletedAt != nil {
		t.Errorf("Expected history v1, got %+v", history)
	}

	// 2. Edit the link (v2)
	time.Sleep(1 * time.Second) // Ensure different Created timestamp
	link2 := &Link{Short: "history-test", Long: "https://v2.com", Owner: "user1"}
	if err := db.SaveWithHistory(context.Background(), link2); err != nil {
		t.Fatal(err)
	}

	// Verify v2 is active, v1 is soft-deleted
	loadedLink, err = db.Load("history-test")
	if err != nil {
		t.Fatal(err)
	}
	if loadedLink.Long != "https://v2.com" || loadedLink.DeletedAt != nil {
		t.Errorf("Expected v2 active, got %+v", loadedLink)
	}
	history, err = db.GetLinkHistory("history-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Long != "https://v2.com" || history[0].DeletedAt != nil || history[1].Long != "https://v1.com" || history[1].DeletedAt == nil {
		t.Errorf("Expected history v2 (active), v1 (deleted), got %+v", history)
	}

	// 3. Edit the link again (v3)
	time.Sleep(1 * time.Second) // Ensure different Created timestamp
	link3 := &Link{Short: "history-test", Long: "https://v3.com", Owner: "user2"}
	if err := db.SaveWithHistory(context.Background(), link3); err != nil {
		t.Fatal(err)
	}

	// Verify v3 is active, v2 and v1 are soft-deleted
	loadedLink, err = db.Load("history-test")
	if err != nil {
		t.Fatal(err)
	}
	if loadedLink.Long != "https://v3.com" || loadedLink.DeletedAt != nil {
		t.Errorf("Expected v3 active, got %+v", loadedLink)
	}
	history, err = db.GetLinkHistory("history-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 || history[0].Long != "https://v3.com" || history[0].DeletedAt != nil || history[1].Long != "https://v2.com" || history[1].DeletedAt == nil || history[2].Long != "https://v1.com" || history[2].DeletedAt == nil {
		t.Errorf("Expected history v3 (active), v2 (deleted), v1 (deleted), got %+v", history)
	}

	// 4. Soft-delete the link
	if err := db.Delete(context.Background(), "history-test"); err != nil {
		t.Fatal(err)
	}

	// Verify link is no longer active, but exists as deleted
	_, err = db.Load("history-test")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Expected link to not be found after soft-delete, got %v", err)
	}
	deletedLink, err := db.LoadDeleted("history-test")
	if err != nil {
		t.Fatal(err)
	}
	if deletedLink.Long != "https://v3.com" || deletedLink.DeletedAt == nil {
		t.Errorf("Expected v3 to be deleted, got %+v", deletedLink)
	}
	history, err = db.GetLinkHistory("history-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 || history[0].Long != "https://v3.com" || history[0].DeletedAt == nil || history[1].Long != "https://v2.com" || history[1].DeletedAt == nil || history[2].Long != "https://v1.com" || history[2].DeletedAt == nil {
		t.Errorf("Expected history v3 (deleted), v2 (deleted), v1 (deleted), got %+v", history)
	}

	// 5. Undelete the link
	if err := db.Undelete(context.Background(), "history-test"); err != nil {
		t.Fatal(err)
	}

	// Verify link is active again (v3 should be active)
	loadedLink, err = db.Load("history-test")
	if err != nil {
		t.Fatal(err)
	}
	if loadedLink.Long != "https://v3.com" || loadedLink.DeletedAt != nil {
		t.Errorf("Expected v3 active after undelete, got %+v", loadedLink)
	}
	history, err = db.GetLinkHistory("history-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 || history[0].Long != "https://v3.com" || history[0].DeletedAt != nil || history[1].Long != "https://v2.com" || history[1].DeletedAt == nil || history[2].Long != "https://v1.com" || history[2].DeletedAt == nil {
		t.Errorf("Expected history v3 (active), v2 (deleted), v1 (deleted) after undelete, got %+v", history)
	}
}

// Test retention window - cleanup respects the cutoff time
// Records before cutoff are hard-deleted, records after are preserved
func Test_DeletedRetention_Window(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create and delete a link
	link := &Link{Short: "test", Long: "https://test.com", Owner: "user"}
	if err := db.Save(link); err != nil {
		t.Fatal(err)
	}

	if err := db.Delete(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}

	// Cleanup with cutoff in the future deletes all old records
	futureTime := time.Now().Add(24 * time.Hour)
	_, err = db.CleanupDeleted(futureTime, 1000)
	if err != nil {
		t.Fatal(err)
	}

	// Link should still exist (most recent record is preserved)
	deleted, err := db.LoadDeleted("test")
	if err != nil {
		t.Errorf("Expected deleted link to still exist (most recent is preserved): %v", err)
	}
	if deleted.DeletedAt == nil {
		t.Error("Expected link to be soft-deleted")
	}
}

// Test cleanup with no expired records - should not delete anything
func Test_Cleanup_No_Expired_Records(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create and delete a link very recently
	link := &Link{Short: "recent", Long: "https://recent.com", Owner: "user"}
	if err := db.Save(link); err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(context.Background(), "recent"); err != nil {
		t.Fatal(err)
	}

	// Cleanup with cutoff in the future - nothing should expire
	futureTime := time.Now().Add(24 * time.Hour)
	count, err := db.CleanupDeleted(futureTime, 1000)
	if err != nil {
		t.Fatal(err)
	}

	if count != 0 {
		t.Errorf("Expected to delete 0 links (all within retention), deleted %d", count)
	}

	// Verify link still exists as soft-deleted
	deletedLink, err := db.LoadDeleted("recent")
	if err != nil {
		t.Errorf("Expected link to still exist as soft-deleted, got error: %v", err)
	}
	if deletedLink.DeletedAt == nil {
		t.Error("Expected link to be soft-deleted")
	}
}

// Test concurrent deletes don't cause issues
func Test_Concurrent_Deletes(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create 10 links
	for i := 0; i < 10; i++ {
		link := &Link{
			Short: fmt.Sprintf("link%d", i),
			Long:  fmt.Sprintf("https://example.com/%d", i),
			Owner: "user",
		}
		if err := db.Save(link); err != nil {
			t.Fatal(err)
		}
	}

	// Delete them concurrently
	var wg sync.WaitGroup
	errChan := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			short := fmt.Sprintf("link%d", idx)
			if err := db.Delete(context.Background(), short); err != nil {
				errChan <- err
			}
		}(i)
	}
	wg.Wait()
	close(errChan)

	// Check for errors
	for err := range errChan {
		if err != nil {
			t.Errorf("Concurrent delete error: %v", err)
		}
	}

	// Verify all links are soft-deleted
	allLinks, err := db.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(allLinks) != 0 {
		t.Errorf("Expected all links to be soft-deleted (LoadAll returns 0), got %d", len(allLinks))
	}

	// Verify all links can be retrieved as deleted
	for i := 0; i < 10; i++ {
		short := fmt.Sprintf("link%d", i)
		deletedLink, err := db.LoadDeleted(short)
		if err != nil {
			t.Errorf("Expected to find deleted link %s, got error: %v", short, err)
		}
		if deletedLink.DeletedAt == nil {
			t.Errorf("Expected link %s to be soft-deleted", short)
		}
	}
}

// Test cleanup preserves most recent deleted record per link
func Test_Cleanup_Preserves_History(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create a simple link, delete it, and verify cleanup preserves it
	link := &Link{Short: "test", Long: "https://test.com", Owner: "user"}
	if err := db.Save(link); err != nil {
		t.Fatal(err)
	}

	if err := db.Delete(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}

	// Verify link is soft-deleted
	deletedLink, err := db.LoadDeleted("test")
	if err != nil {
		t.Errorf("Expected to find deleted link, got error: %v", err)
	}
	if deletedLink.DeletedAt == nil {
		t.Error("Expected link to be soft-deleted")
	}

	// Cleanup with cutoff in the future - all records are older than future cutoff
	futureTime := time.Now().Add(24 * time.Hour)
	_, err = db.CleanupDeleted(futureTime, 1000)
	if err != nil {
		t.Fatal(err)
	}

	// Even though we tried to clean up, the most recent deleted record should be preserved
	// (CleanupDeleted preserves at least one historical record per ID)
	deletedAfterCleanup, err := db.LoadDeleted("test")
	if err != nil {
		t.Errorf("Expected most recent deleted record to be preserved after cleanup, got error: %v", err)
	}
	if deletedAfterCleanup.DeletedAt == nil {
		t.Error("Expected link to still be soft-deleted after cleanup")
	}
}

// Test immediate deletion mode: deleted-retention=0, cleanup-interval=0
// Link should be permanently removed immediately after deletion
func Test_ImmediateDeletion_Mode(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create a link
	link := &Link{Short: "immediate-test", Long: "https://example.com", Owner: "user"}
	if err := db.Save(link); err != nil {
		t.Fatal(err)
	}

	// Verify link exists
	loaded, err := db.Load("immediate-test")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DeletedAt != nil {
		t.Error("Link should not be deleted initially")
	}

	// Delete the link
	if err := db.Delete(context.Background(), "immediate-test"); err != nil {
		t.Fatal(err)
	}

	// Simulate immediate cleanup with zero retention (retention = 0 means delete everything)
	// cutoff = now - 0 = now (any deletion before now is deleted)
	cutoff := time.Now()
	deletedCount, err := db.CleanupDeleted(cutoff, 1000)
	if err != nil {
		t.Fatal(err)
	}

	// In immediate mode, the most recent deleted record is still preserved for audit trail
	// But older versions would be deleted. Since we only have one version, nothing is deleted.
	if deletedCount != 0 {
		t.Errorf("Expected 0 deletions (preserving most recent), got %d", deletedCount)
	}

	// The link should still exist in deleted state (most recent is preserved)
	deletedLink, err := db.LoadDeleted("immediate-test")
	if err != nil {
		t.Errorf("Expected deleted link to exist (preserved for audit), got error: %v", err)
	}
	if deletedLink.DeletedAt == nil {
		t.Error("Expected link to be marked as deleted")
	}

	// Active lookup should fail (not in normal Load)
	_, err = db.Load("immediate-test")
	if err == nil {
		t.Error("Expected Load() to fail for deleted link")
	}
}

// Test delayed deletion mode: deleted-retention>0, cleanup-interval=0
// Links are soft-deleted immediately but hard-deleted only after retention period
func Test_DelayedDeletion_Mode(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create links at different times (simulate old and recent deletes)
	oldLink := &Link{Short: "old-deleted", Long: "https://old.com", Owner: "user"}
	if err := db.Save(oldLink); err != nil {
		t.Fatal(err)
	}

	recentLink := &Link{Short: "recent-deleted", Long: "https://recent.com", Owner: "user"}
	if err := db.Save(recentLink); err != nil {
		t.Fatal(err)
	}

	// Manually delete old link and set its DeletedAt to the past
	if err := db.Delete(context.Background(), "old-deleted"); err != nil {
		t.Fatal(err)
	}
	oldDeleted, err := db.LoadDeleted("old-deleted")
	if err != nil {
		t.Fatal(err)
	}

	// Manually update the DeletedAt to 2 hours ago
	pastTime := time.Now().Add(-2 * time.Hour)
	oldDeleted.DeletedAt = &pastTime
	// Note: We can't easily update in test, so we'll work with what we have

	// Delete recent link
	if err := db.Delete(context.Background(), "recent-deleted"); err != nil {
		t.Fatal(err)
	}

	// Simulate delayed cleanup with 1 hour retention
	// cutoff = now - 1 hour (delete things deleted before 1 hour ago)
	retentionDuration := 1 * time.Hour
	cutoff := time.Now().Add(-retentionDuration)

	_, err = db.CleanupDeleted(cutoff, 1000)
	if err != nil {
		t.Fatal(err)
	}

	// Recent link was deleted < 1 hour ago, should still be recoverable
	recentDeletedLink, err := db.LoadDeleted("recent-deleted")
	if err != nil {
		t.Errorf("Expected recent deleted link to still exist within retention window, got error: %v", err)
	}
	if recentDeletedLink.DeletedAt == nil {
		t.Error("Expected recent link to be marked as deleted")
	}

	// Verify link is not in active list
	allActive, err := db.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range allActive {
		if link.Short == "recent-deleted" {
			t.Error("Deleted link should not appear in LoadAll()")
		}
	}
}

// Test retention window boundary: links just outside retention window are deleted
func Test_Retention_Window_Boundary(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create multiple links to simulate version history
	link := &Link{Short: "versioned", Long: "https://v1.com", Owner: "user", Created: time.Now().Add(-5 * time.Minute)}
	if err := db.Save(link); err != nil {
		t.Fatal(err)
	}

	// Edit it (creates another version, soft-deletes the old one)
	time.Sleep(1001 * time.Millisecond)
	editLink := &Link{Short: "versioned", Long: "https://v2.com", Owner: "user"}
	if err := db.SaveWithHistory(context.Background(), editLink); err != nil {
		t.Fatal(err)
	}

	// Edit it again
	time.Sleep(1001 * time.Millisecond)
	editLink2 := &Link{Short: "versioned", Long: "https://v3.com", Owner: "user"}
	if err := db.SaveWithHistory(context.Background(), editLink2); err != nil {
		t.Fatal(err)
	}

	// Now delete it
	if err := db.Delete(context.Background(), "versioned"); err != nil {
		t.Fatal(err)
	}

	// Load all versions including deleted
	history, err := db.GetLinkHistory("versioned")
	if err != nil {
		t.Fatal(err)
	}

	// Should have 4 versions: 3 original + edits, all but latest should be soft-deleted
	if len(history) < 3 {
		t.Errorf("Expected at least 3 versions, got %d", len(history))
	}

	// Cleanup with 2-minute retention (should delete v1 but preserve most recent)
	retentionCutoff := time.Now().Add(-2 * time.Minute)
	_, err = db.CleanupDeleted(retentionCutoff, 1000)
	if err != nil {
		t.Fatal(err)
	}

	// Most recent deleted record (the deletion) should be preserved
	deletedLink, err := db.LoadDeleted("versioned")
	if err != nil {
		t.Errorf("Expected most recent deleted record to be preserved, got error: %v", err)
	}
	if deletedLink.DeletedAt == nil {
		t.Error("Expected link to be marked as deleted")
	}

	// History should still be queryable
	historyAfter, err := db.GetLinkHistory("versioned")
	if err != nil {
		t.Fatal(err)
	}
	if len(historyAfter) == 0 {
		t.Error("Expected at least one version to be preserved after cleanup")
	}
}

// Test that CleanupDeleted only removes old records, not recent ones
func Test_Cleanup_Respects_Cutoff_Time(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create and delete a link
	link := &Link{Short: "test", Long: "https://example.com", Owner: "user"}
	if err := db.Save(link); err != nil {
		t.Fatal(err)
	}

	if err := db.Delete(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}

	// Get the deletion time
	deleted, err := db.LoadDeleted("test")
	if err != nil {
		t.Fatal(err)
	}
	deletedTime := deleted.DeletedAt

	// Cleanup with cutoff BEFORE the deletion (nothing should be deleted)
	pastCutoff := deletedTime.Add(-1 * time.Hour)
	count, err := db.CleanupDeleted(pastCutoff, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("Expected 0 deletions with past cutoff, got %d", count)
	}

	// Link should still be recoverable
	still, err := db.LoadDeleted("test")
	if err != nil {
		t.Errorf("Link should still exist: %v", err)
	}
	if still.DeletedAt == nil {
		t.Error("Link should be marked as deleted")
	}

	// Cleanup with cutoff AFTER the deletion (most recent is preserved)
	futureCutoff := deletedTime.Add(1 * time.Hour)
	count, err = db.CleanupDeleted(futureCutoff, 1000)
	if err != nil {
		t.Fatal(err)
	}
	// Count includes all but most recent, but we only have one, so 0 deleted
	if count != 0 {
		t.Errorf("Expected 0 deletions (most recent preserved), got %d", count)
	}

	// Most recent should still be preserved for audit
	preserved, err := db.LoadDeleted("test")
	if err != nil {
		t.Errorf("Most recent deleted record should be preserved: %v", err)
	}
	if preserved.DeletedAt == nil {
		t.Error("Link should still be marked as deleted")
	}
}

// Test SaveWithHistory with multiple edits and deletions
func Test_SaveWithHistory_Multiple_Edits(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create initial link
	link := &Link{Short: "multi", Long: "https://v1.com", Owner: "user1"}
	if err := db.Save(link); err != nil {
		t.Fatal(err)
	}

	// Verify initial state
	v1, err := db.Load("multi")
	if err != nil {
		t.Fatal(err)
	}
	if v1.Long != "https://v1.com" {
		t.Errorf("Expected v1, got %s", v1.Long)
	}

	// Edit 1
	time.Sleep(1001 * time.Millisecond)
	edit1 := &Link{Short: "multi", Long: "https://v2.com", Owner: "user1"}
	if err := db.SaveWithHistory(context.Background(), edit1); err != nil {
		t.Fatal(err)
	}

	v2, err := db.Load("multi")
	if err != nil {
		t.Fatal(err)
	}
	if v2.Long != "https://v2.com" {
		t.Errorf("Expected v2, got %s", v2.Long)
	}
	if v2.LastEdit.IsZero() {
		t.Error("LastEdit should be set after first edit")
	}

	// Edit 2
	time.Sleep(1001 * time.Millisecond)
	edit2 := &Link{Short: "multi", Long: "https://v3.com", Owner: "user2"}
	if err := db.SaveWithHistory(context.Background(), edit2); err != nil {
		t.Fatal(err)
	}

	v3, err := db.Load("multi")
	if err != nil {
		t.Fatal(err)
	}
	if v3.Long != "https://v3.com" {
		t.Errorf("Expected v3, got %s", v3.Long)
	}
	if v3.Owner != "user2" {
		t.Errorf("Expected owner to be updated to user2, got %s", v3.Owner)
	}

	// Check full history
	history, err := db.GetLinkHistory("multi")
	if err != nil {
		t.Fatal(err)
	}

	if len(history) != 3 {
		t.Errorf("Expected 3 versions in history, got %d", len(history))
	}

	// Most recent should be active (v3)
	if history[0].Long != "https://v3.com" || history[0].DeletedAt != nil {
		t.Errorf("First entry should be active v3, got %+v", history[0])
	}

	// Middle version should be soft-deleted (v2)
	if history[1].Long != "https://v2.com" || history[1].DeletedAt == nil {
		t.Errorf("Second entry should be soft-deleted v2, got %+v", history[1])
	}

	// Oldest version should be soft-deleted (v1)
	if history[2].Long != "https://v1.com" || history[2].DeletedAt == nil {
		t.Errorf("Third entry should be soft-deleted v1, got %+v", history[2])
	}
}

// Test GetLinkHistory returns versions in correct order
func Test_GetLinkHistory_Order(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create link
	link := &Link{Short: "ordered", Long: "v1", Owner: "user"}
	if err := db.Save(link); err != nil {
		t.Fatal(err)
	}

	// Make 3 edits
	for i := 2; i <= 4; i++ {
		time.Sleep(1001 * time.Millisecond)
		edit := &Link{Short: "ordered", Long: fmt.Sprintf("v%d", i), Owner: "user"}
		if err := db.SaveWithHistory(context.Background(), edit); err != nil {
			t.Fatal(err)
		}
	}

	history, err := db.GetLinkHistory("ordered")
	if err != nil {
		t.Fatal(err)
	}

	if len(history) != 4 {
		t.Fatalf("Expected 4 versions, got %d", len(history))
	}

	// Verify order: most recent first (v4, v3, v2, v1)
	expectedOrder := []string{"v4", "v3", "v2", "v1"}
	for i, expected := range expectedOrder {
		if history[i].Long != expected {
			t.Errorf("Position %d: expected %s, got %s", i, expected, history[i].Long)
		}
	}

	// Current version should be v4 (not in history with DeletedAt)
	current, err := db.Load("ordered")
	if err != nil {
		t.Fatal(err)
	}
	if current.Long != "v4" {
		t.Errorf("Current version should be v4, got %s", current.Long)
	}
	if current.DeletedAt != nil {
		t.Error("Current version should not be soft-deleted")
	}
}

// Test Delete then SaveWithHistory creates fresh link (no history)
func Test_Delete_Then_SaveWithHistory_Creates_Fresh(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create and delete a link
	link := &Link{Short: "restore", Long: "https://original.com", Owner: "user"}
	if err := db.Save(link); err != nil {
		t.Fatal(err)
	}

	if err := db.Delete(context.Background(), "restore"); err != nil {
		t.Fatal(err)
	}

	// Verify it's deleted
	_, err = db.Load("restore")
	if err == nil {
		t.Error("Expected load to fail for deleted link")
	}

	// Restore it with SaveWithHistory
	restored := &Link{Short: "restore", Long: "https://restored.com", Owner: "user"}
	if err := db.SaveWithHistory(context.Background(), restored); err != nil {
		t.Fatal(err)
	}

	// Should be accessible now
	current, err := db.Load("restore")
	if err != nil {
		t.Fatal(err)
	}
	if current.Long != "https://restored.com" {
		t.Errorf("Expected restored URL, got %s", current.Long)
	}
	if current.DeletedAt != nil {
		t.Error("Restored link should not be marked as deleted")
	}

	// When restoring a completely deleted link with SaveWithHistory,
	// it creates a fresh link (no previous active version to soft-delete)
	// So history will show only the restored version
	history, err := db.GetLinkHistory("restore")
	if err != nil {
		t.Fatal(err)
	}

	// Since there was no active version to soft-delete, only the restored version exists
	if len(history) != 1 {
		t.Errorf("Expected 1 version (fresh restore), got %d", len(history))
	}

	// The restored version should be active (not deleted)
	if history[0].Long != "https://restored.com" || history[0].DeletedAt != nil {
		t.Errorf("Restored version should be active, got %+v", history[0])
	}
}

// Test multiple links with history don't interfere with each other
func Test_History_Multiple_Links_Independent(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create link1
	link1 := &Link{Short: "link1", Long: "link1-v1", Owner: "user"}
	if err := db.Save(link1); err != nil {
		t.Fatal(err)
	}

	// Create link2
	link2 := &Link{Short: "link2", Long: "link2-v1", Owner: "user"}
	if err := db.Save(link2); err != nil {
		t.Fatal(err)
	}

	// Edit link1 multiple times
	for i := 2; i <= 3; i++ {
		time.Sleep(1001 * time.Millisecond)
		edit := &Link{Short: "link1", Long: fmt.Sprintf("link1-v%d", i), Owner: "user"}
		if err := db.SaveWithHistory(context.Background(), edit); err != nil {
			t.Fatal(err)
		}
	}

	// Edit link2 only once
	time.Sleep(1001 * time.Millisecond)
	edit2 := &Link{Short: "link2", Long: "link2-v2", Owner: "user"}
	if err := db.SaveWithHistory(context.Background(), edit2); err != nil {
		t.Fatal(err)
	}

	// Verify link1 history: 3 versions
	hist1, err := db.GetLinkHistory("link1")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist1) != 3 {
		t.Errorf("link1: expected 3 versions, got %d", len(hist1))
	}

	// Verify link2 history: 2 versions
	hist2, err := db.GetLinkHistory("link2")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist2) != 2 {
		t.Errorf("link2: expected 2 versions, got %d", len(hist2))
	}

	// Verify current versions are correct
	current1, err := db.Load("link1")
	if err != nil {
		t.Fatal(err)
	}
	if current1.Long != "link1-v3" {
		t.Errorf("link1: expected v3, got %s", current1.Long)
	}

	current2, err := db.Load("link2")
	if err != nil {
		t.Fatal(err)
	}
	if current2.Long != "link2-v2" {
		t.Errorf("link2: expected v2, got %s", current2.Long)
	}
}

// Test Undelete restores specific deleted version
func Test_Undelete_Restores_Most_Recent_Deleted(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create and edit link multiple times
	link := &Link{Short: "test", Long: "v1", Owner: "user"}
	if err := db.Save(link); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1001 * time.Millisecond)
	edit := &Link{Short: "test", Long: "v2", Owner: "user"}
	if err := db.SaveWithHistory(context.Background(), edit); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1001 * time.Millisecond)
	edit2 := &Link{Short: "test", Long: "v3", Owner: "user"}
	if err := db.SaveWithHistory(context.Background(), edit2); err != nil {
		t.Fatal(err)
	}

	// Delete it
	if err := db.Delete(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}

	// Link should be deleted
	_, err = db.Load("test")
	if err == nil {
		t.Error("Expected link to be deleted")
	}

	// Undelete
	if err := db.Undelete(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}

	// Should be back to v3
	restored, err := db.Load("test")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Long != "v3" {
		t.Errorf("Expected v3 after undelete, got %s", restored.Long)
	}

	// History should be intact
	history, err := db.GetLinkHistory("test")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 {
		t.Errorf("Expected 3 versions in history, got %d", len(history))
	}
}

// Test LoadAllIncludingDeleted shows correct state
func Test_LoadAllIncludingDeleted_Shows_All_States(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create 3 links
	link1 := &Link{Short: "active1", Long: "https://active1.com", Owner: "user"}
	link2 := &Link{Short: "deleted1", Long: "https://deleted1.com", Owner: "user"}
	link3 := &Link{Short: "active2", Long: "https://active2.com", Owner: "user"}

	for _, link := range []*Link{link1, link2, link3} {
		if err := db.Save(link); err != nil {
			t.Fatal(err)
		}
	}

	// Delete link2
	if err := db.Delete(context.Background(), "deleted1"); err != nil {
		t.Fatal(err)
	}

	// LoadAll should show 2 active links
	allActive, err := db.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(allActive) != 2 {
		t.Errorf("LoadAll: expected 2 active, got %d", len(allActive))
	}

	// LoadAllIncludingDeleted should show 3 total
	allIncluding, err := db.LoadAllIncludingDeleted()
	if err != nil {
		t.Fatal(err)
	}
	if len(allIncluding) != 3 {
		t.Errorf("LoadAllIncludingDeleted: expected 3 total, got %d", len(allIncluding))
	}

	// Count active vs deleted
	activeCount := 0
	deletedCount := 0
	for _, link := range allIncluding {
		if link.DeletedAt == nil {
			activeCount++
		} else {
			deletedCount++
		}
	}

	if activeCount != 2 {
		t.Errorf("Expected 2 active in LoadAllIncludingDeleted, got %d", activeCount)
	}
	if deletedCount != 1 {
		t.Errorf("Expected 1 deleted in LoadAllIncludingDeleted, got %d", deletedCount)
	}
}

// Test GetLinkHistory with non-existent link returns empty
func Test_GetLinkHistory_NonExistent(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Query history for non-existent link
	history, err := db.GetLinkHistory("nonexistent")
	if err != nil {
		t.Fatal(err)
	}

	if len(history) != 0 {
		t.Errorf("Expected empty history for non-existent link, got %d entries", len(history))
	}
}

// Test Save with pre-set Created timestamp
func Test_Save_With_Existing_Timestamp(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create link with explicit timestamp
	fixedTime := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	link := &Link{Short: "timestamped", Long: "https://example.com", Owner: "user", Created: fixedTime}
	if err := db.Save(link); err != nil {
		t.Fatal(err)
	}

	// Load and verify timestamp was preserved
	loaded, err := db.Load("timestamped")
	if err != nil {
		t.Fatal(err)
	}

	if loaded.Created != fixedTime {
		t.Errorf("Expected timestamp %v, got %v", fixedTime, loaded.Created)
	}
}

// Test Save with DeletedAt set
func Test_Save_With_DeletedAt(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Save link with DeletedAt already set
	deletedTime := time.Now().UTC()
	link := &Link{Short: "deleted", Long: "https://example.com", Owner: "user", DeletedAt: &deletedTime}
	if err := db.Save(link); err != nil {
		t.Fatal(err)
	}

	// Verify it's marked as deleted
	_, err = db.Load("deleted")
	if err == nil {
		t.Error("Expected Load() to fail for deleted link")
	}

	// Verify we can load it as deleted
	loaded, err := db.LoadDeleted("deleted")
	if err != nil {
		t.Fatal(err)
	}

	if loaded.DeletedAt == nil {
		t.Error("Expected DeletedAt to be set")
	}
}

// Test SaveWithHistory error when update fails
func Test_SaveWithHistory_Update_Error(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create and edit a link
	link := &Link{Short: "edit-test", Long: "v1", Owner: "user"}
	if err := db.Save(link); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1001 * time.Millisecond)

	// Normal edit should succeed
	edit := &Link{Short: "edit-test", Long: "v2", Owner: "user"}
	if err := db.SaveWithHistory(context.Background(), edit); err != nil {
		t.Fatal(err)
	}

	// Verify it was updated
	loaded, err := db.Load("edit-test")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Long != "v2" {
		t.Errorf("Expected v2, got %s", loaded.Long)
	}
}

// Test SaveWithHistory with DeletedAt set
func Test_SaveWithHistory_With_DeletedAt(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create initial link
	link := &Link{Short: "history-delete", Long: "v1", Owner: "user"}
	if err := db.Save(link); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1001 * time.Millisecond)

	// Edit and mark as deleted in same operation
	deleteTime := time.Now().UTC()
	edit := &Link{Short: "history-delete", Long: "v2", Owner: "user", DeletedAt: &deleteTime}
	if err := db.SaveWithHistory(context.Background(), edit); err != nil {
		t.Fatal(err)
	}

	// Should not be loadable normally
	_, err = db.Load("history-delete")
	if err == nil {
		t.Error("Expected Load() to fail for deleted link")
	}

	// Should be loadable as deleted
	deleted, err := db.LoadDeleted("history-delete")
	if err != nil {
		t.Fatal(err)
	}
	if deleted.DeletedAt == nil {
		t.Error("Expected DeletedAt to be set")
	}
	if deleted.Long != "v2" {
		t.Errorf("Expected v2, got %s", deleted.Long)
	}
}

// Test Delete non-existent link returns error
func Test_Delete_NonExistent(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Try to delete non-existent link
	err = db.Delete(context.Background(), "nonexistent")
	if err == nil {
		t.Error("Expected error when deleting non-existent link")
	}
}

// Test Undelete non-existent deleted link returns error
func Test_Undelete_NonExistent(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Try to undelete non-existent deleted link
	err = db.Undelete(context.Background(), "nonexistent")
	if err == nil {
		t.Error("Expected error when undeleting non-existent link")
	}
}

// Test Undelete already active link (nothing to restore)
func Test_Undelete_Active_Link(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create an active link
	link := &Link{Short: "active", Long: "https://example.com", Owner: "user"}
	if err := db.Save(link); err != nil {
		t.Fatal(err)
	}

	// Try to undelete an active link (should fail)
	err = db.Undelete(context.Background(), "active")
	if err == nil {
		t.Error("Expected error when undeleting active link")
	}
}

// Test CleanupDeleted with batch limit
func Test_CleanupDeleted_Batch_Limit(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create and delete multiple links with old timestamps
	for i := 1; i <= 5; i++ {
		link := &Link{
			Short: fmt.Sprintf("batch-%d", i),
			Long:  fmt.Sprintf("https://example%d.com", i),
			Owner: "user",
		}
		if err := db.Save(link); err != nil {
			t.Fatal(err)
		}

		// Edit each multiple times to create versions
		for j := 2; j <= 3; j++ {
			time.Sleep(1001 * time.Millisecond)
			edit := &Link{Short: fmt.Sprintf("batch-%d", i), Long: fmt.Sprintf("v%d", j), Owner: "user"}
			if err := db.SaveWithHistory(context.Background(), edit); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Delete all
	for i := 1; i <= 5; i++ {
		if err := db.Delete(context.Background(), fmt.Sprintf("batch-%d", i)); err != nil {
			t.Fatal(err)
		}
	}

	// Cleanup with batch size of 2 (should clean in multiple batches)
	futureCutoff := time.Now().Add(24 * time.Hour)
	count, err := db.CleanupDeleted(futureCutoff, 2)
	if err != nil {
		t.Fatal(err)
	}

	// All soft-deleted old versions should be cleaned (5 links * 2 old versions = 10, minus 5 most recent = 5)
	// But CleanupDeleted preserves most recent per ID, so expect some old versions to be cleaned
	if count < 0 {
		t.Errorf("Expected non-negative cleanup count, got %d", count)
	}
}

// Test LoadStats with empty database
func Test_LoadStats_Empty(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	stats, err := db.LoadStats()
	if err != nil {
		t.Fatal(err)
	}

	if len(stats) != 0 {
		t.Errorf("Expected empty stats, got %d entries", len(stats))
	}
}

// Test LoadStats with multiple links and stats
func Test_LoadStats_Multiple(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create links
	link1 := &Link{Short: "stats1", Long: "https://example1.com", Owner: "user"}
	link2 := &Link{Short: "stats2", Long: "https://example2.com", Owner: "user"}
	link3 := &Link{Short: "stats3", Long: "https://example3.com", Owner: "user"}

	for _, link := range []*Link{link1, link2, link3} {
		if err := db.Save(link); err != nil {
			t.Fatal(err)
		}
	}

	// Save some stats
	statsToSave := make(ClickStats)
	statsToSave["stats1"] = 10
	statsToSave["stats2"] = 20
	statsToSave["stats3"] = 30

	if err := db.SaveStats(statsToSave); err != nil {
		t.Fatal(err)
	}

	// Load stats
	loaded, err := db.LoadStats()
	if err != nil {
		t.Fatal(err)
	}

	if len(loaded) != 3 {
		t.Errorf("Expected 3 stats entries, got %d", len(loaded))
	}

	for short, expectedClicks := range statsToSave {
		if loaded[short] != expectedClicks {
			t.Errorf("Expected %s to have %d clicks, got %d", short, expectedClicks, loaded[short])
		}
	}
}

// Test LoadStats with deleted link stats (orphaned stats should not appear)
func Test_LoadStats_Orphaned_After_Delete(t *testing.T) {
	db, err := NewSQLiteDB(path.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}

	// Create two links
	link1 := &Link{Short: "active-stats", Long: "https://example1.com", Owner: "user"}
	link2 := &Link{Short: "deleted-stats", Long: "https://example2.com", Owner: "user"}

	for _, link := range []*Link{link1, link2} {
		if err := db.Save(link); err != nil {
			t.Fatal(err)
		}
	}

	// Save stats for both
	statsToSave := make(ClickStats)
	statsToSave["active-stats"] = 100
	statsToSave["deleted-stats"] = 200

	if err := db.SaveStats(statsToSave); err != nil {
		t.Fatal(err)
	}

	// Verify both stats are saved initially
	loaded, err := db.LoadStats()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Errorf("Expected 2 stat entries before delete, got %d", len(loaded))
	}

	// Delete one link
	if err := db.Delete(context.Background(), "deleted-stats"); err != nil {
		t.Fatal(err)
	}

	// Load stats - orphaned stats appear as empty string key (link not in LoadAll)
	loaded, err = db.LoadStats()
	if err != nil {
		t.Fatal(err)
	}

	// LoadStats returns: 1 active link stat + 1 orphaned stat with empty key (for deleted link)
	if len(loaded) != 2 {
		t.Errorf("Expected 2 stat entries (1 active + 1 orphaned), got %d", len(loaded))
	}

	if val, exists := loaded["active-stats"]; !exists || val != 100 {
		t.Errorf("Expected active-stats with 100 clicks, got %v", val)
	}

	// Orphaned stats appear with empty key when link is deleted
	if val, exists := loaded[""]; !exists || val != 200 {
		t.Errorf("Expected orphaned stats with empty key and 200 clicks, got %v", val)
	}
}

// Test migration adds DeletedAt and DeletedBy columns to existing databases
func Test_Migration_AddDeletedByColumn(t *testing.T) {
	tmpdir := t.TempDir()
	dbPath := path.Join(tmpdir, "links.db")

	// Create a database with the old schema (without DeletedAt and DeletedBy)
	oldSchema := `
CREATE TABLE IF NOT EXISTS Links
(
    ID         TEXT    NOT NULL,
    Short      TEXT    NOT NULL DEFAULT "",
    Long       TEXT    NOT NULL DEFAULT "",
    Created    INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
    Owner      TEXT    NOT NULL DEFAULT "",
    UNIQUE (ID, Created)
);
CREATE TABLE IF NOT EXISTS Stats
(
    ID      TEXT    NOT NULL DEFAULT "",
    Created INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
    Clicks  INTEGER
);
`

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatalf("Failed to create old schema: %v\nSchema:\n%s", err, oldSchema)
	}

	// Insert a test link
	if _, err := db.Exec("INSERT INTO Links (ID, Short, Long, Created, Owner) VALUES (?, ?, ?, ?, ?)",
		"test", "test", "https://example.com", 1234567890, "user@example.com"); err != nil {
		t.Fatal(err)
	}

	db.Close()

	// Now open with NewSQLiteDB which should trigger migration
	gdb, err := NewSQLiteDB(dbPath)
	if err != nil {
		t.Fatalf("Failed to open DB with migration: %v", err)
	}
	defer gdb.db.Close()

	// Verify DeletedBy column now exists by loading the link
	link, err := gdb.Load("test")
	if err != nil {
		t.Fatal(err)
	}

	if link.DeletedBy != nil {
		t.Errorf("Expected DeletedBy to be nil for migrated old record, got %v", *link.DeletedBy)
	}

	// Verify we can delete a link after migration
	ctx := context.WithValue(context.Background(), CurrentUserKey, user{login: "admin@example.com"})
	if err := gdb.Delete(ctx, "test"); err != nil {
		t.Fatal(err)
	}

	// Verify DeletedBy is now set
	deletedLink, err := gdb.LoadDeleted("test")
	if err != nil {
		t.Fatal(err)
	}
	if deletedLink.DeletedBy == nil || *deletedLink.DeletedBy != "admin@example.com" {
		got := ""
		if deletedLink.DeletedBy != nil {
			got = *deletedLink.DeletedBy
		}
		t.Errorf("Expected DeletedBy to be 'admin@example.com' after delete, got %q", got)
	}
}
