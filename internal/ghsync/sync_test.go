// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ghsync

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aclements/ghissues/internal/mockserver"
)

type testReporter struct {
	t *testing.T

	progress string
}

func (r *testReporter) Logf(format string, args ...any) {
	r.t.Helper()
	r.t.Logf(format, args...)
}

func (r *testReporter) Progress(msg string) {
	r.t.Helper()
	if r.progress == msg {
		return
	}
	r.progress = msg
	r.t.Log(msg)
}

// ProgressDone is called when a task is completed, indicating its final status.
func (r *testReporter) ProgressDone(msg string, status string) {
	r.t.Helper()
	r.t.Logf("%s %s", msg, status)
	r.progress = ""
}

func setTestPerPage(t *testing.T, n int) {
	old := perPage
	perPage = n
	t.Cleanup(func() { perPage = old })
}

func TestSyncBasic(t *testing.T) {
	setTestPerPage(t, 10)
	ms := mockserver.New(t)

	ms.AddIssues(51)
	ms.AddComments(1, 1)
	ms.AddComments(51, 2)
	ms.AddEvents(1, 1)
	ms.AddEvents(49, 3)

	client := ms.Client(t)
	r := &testReporter{t: t}

	rootDir := t.TempDir()

	// 1. Initial sync
	if err := Sync(client, "owner", "repo", rootDir, r); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	ms.VerifyDir(t, filepath.Join(rootDir, "owner", "repo"))
	// Check that sync attempted backfill. The initial sync also tests events
	// that appear in both the repo-wide stream and the per-issue stream.
	if ms.IssueEventsFetches == 0 {
		t.Errorf("expected issue events requests")
	}
	ms.IssueEventsFetches = 0

	// 2. Sync again with no changes. Check that there are no spurious changes
	// to sync_state.json.
	statePath := filepath.Join(rootDir, "owner", "repo", "sync_state.json")
	beforeState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	if err := Sync(client, "owner", "repo", rootDir, r); err != nil {
		t.Fatalf("Sync (no changes) failed: %v", err)
	}

	afterState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeState, afterState) {
		t.Errorf("sync_state.json changed despite no changes")
	}

	ms.VerifyDir(t, filepath.Join(rootDir, "owner", "repo"))

	if ms.IssueEventsFetches != 0 {
		t.Errorf("unexpected issue events requests")
	}
	ms.IssueEventsFetches = 0

	// 3. Add to corpus
	ms.AddIssues(1)
	ms.AddComments(1, 2)
	ms.AddEvents(1, 2)

	// 4. Sync again and check
	if err := Sync(client, "owner", "repo", rootDir, r); err != nil {
		t.Fatalf("Sync (with changes) failed: %v", err)
	}
	ms.VerifyDir(t, filepath.Join(rootDir, "owner", "repo"))

	// This time there should be no backfill attempt.
	if ms.IssueEventsFetches != 0 {
		t.Errorf("unexpected issue events requests")
	}
}

func TestSyncResume(t *testing.T) {
	setTestPerPage(t, 10)
	// Configure the server to return a non-transient error that will cause the
	// sync loop to exit after a single (new) successful request.
	ms := mockserver.New(t)
	ms.TestResume = true

	ms.AddIssues(51)
	ms.AddComments(1, 1)
	ms.AddComments(51, 2)
	ms.AddEvents(1, 1)
	ms.AddEvents(49, 2)

	client := ms.Client(t)
	r := &testReporter{t: t}

	rootDir := t.TempDir()

	// Loop until sync completes without error
	tries := 0
	maxRetries := 100
	success := false
	for ; tries < maxRetries; tries++ {
		err := Sync(client, "owner", "repo", rootDir, r)

		const maxFetchesPer = 4
		if ms.Fetches > maxFetchesPer {
			// Make sure we're not just starting over from scratch each time and
			// getting a little further.
			t.Fatalf("sync made %d repeated requests; want <= %d", ms.Fetches, maxFetchesPer)
		}
		if err == nil {
			success = true
			break
		}

		t.Logf("sync failed (expected): %s", err)
		// Reset failure for next iteration
		ms.FailAll = false
		ms.Fetches = 0
	}
	t.Logf("sync took %d tries", tries)

	if !success {
		t.Fatalf("Sync failed to complete after %d retries", maxRetries)
	}

	ms.VerifyDir(t, filepath.Join(rootDir, "owner", "repo"))
}

func TestSyncUpdate(t *testing.T) {
	setTestPerPage(t, 10)
	ms := mockserver.New(t)

	ms.AddIssues(1)
	ms.AddComments(1, 1)

	client := ms.Client(t)
	r := &testReporter{t: t}

	rootDir := t.TempDir()

	// 1. Initial sync
	if err := Sync(client, "owner", "repo", rootDir, r); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	ms.VerifyDir(t, filepath.Join(rootDir, "owner", "repo"))

	// 2. Update issue and comment
	ms.Issues[0].Title = "Updated Title"
	ms.Issues[0].UpdatedAt = ms.Issues[0].UpdatedAt.Add(time.Hour)
	ms.Comments[0].Body = "Updated Comment"
	ms.Comments[0].UpdatedAt = ms.Comments[0].UpdatedAt.Add(time.Hour)

	// 3. Sync again
	if err := Sync(client, "owner", "repo", rootDir, r); err != nil {
		t.Fatalf("Sync (with updates) failed: %v", err)
	}
	ms.VerifyDir(t, filepath.Join(rootDir, "owner", "repo"))
}

func TestSyncBackfillBasic(t *testing.T) {
	setTestPerPage(t, 10)
	ms := mockserver.New(t)

	// Add two issues
	ms.AddIssues(2)
	// Add events to both
	ms.AddEvents(21, 1)
	ms.AddEvents(21, 2)

	// Hide the repo-wide events to force backfill.
	ms.ForceBackfill = true

	client := ms.Client(t)
	rootDir := t.TempDir()

	if err := Sync(client, "owner", "repo", rootDir, nil); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	if ms.IssueEventsFetches == 0 {
		t.Errorf("backfill did not happen")
	}

	// Verify that the event was fetched despite being missing from the firehose.
	ms.VerifyDir(t, filepath.Join(rootDir, "owner", "repo"))
}

func TestSyncBackfillETag(t *testing.T) {
	setTestPerPage(t, 10)
	ms := mockserver.New(t)

	// Add an issue and multiple pages of events to it (each page is 10 events, so we add 25)
	ms.AddIssues(1)
	ms.AddEvents(25, 1) // 3 pages: 10, 10, 5

	ms.ForceBackfill = true

	client := ms.Client(t)
	rootDir := t.TempDir()

	// Initial sync
	t.Log("initial sync")
	if err := Sync(client, "owner", "repo", rootDir, nil); err != nil {
		t.Fatalf("First sync failed: %v", err)
	}
	ms.VerifyDir(t, filepath.Join(rootDir, "owner", "repo"))
	if ms.IssueEventsFetches != 3 {
		t.Fatalf("got %d issue event fetches, but expected 3 pages", ms.IssueEventsFetches)
	}

	// Second sync, no changes. All backfill requests should hit ETags.
	t.Log("sync with no changes")
	ms.IssueEventsFetches = 0
	ms.EtagFetches = 0
	if err := Sync(client, "owner", "repo", rootDir, nil); err != nil {
		t.Fatalf("Second sync failed: %v", err)
	}
	if ms.IssueEventsFetches != 0 || ms.EtagFetches != 3 {
		t.Fatalf("expected all 3 backfill fetches to hit etags, but got %d fetches and %d etags hits", ms.IssueEventsFetches, ms.EtagFetches)
	}

	// Add a new event, check that it traverses and finds it.
	t.Log("sync with new event")
	ms.IssueEventsFetches = 0
	ms.EtagFetches = 0
	ms.AddEvents(1, 1) // adding one more event to issue 1
	if err := Sync(client, "owner", "repo", rootDir, nil); err != nil {
		t.Fatalf("Third sync failed: %v", err)
	}
	ms.VerifyDir(t, filepath.Join(rootDir, "owner", "repo"))
	if ms.IssueEventsFetches != 1 || ms.EtagFetches != 2 {
		t.Fatalf("expected 1 new backfill fetch and 2 etags hits, but got %d fetches and %d etags hits", ms.IssueEventsFetches, ms.EtagFetches)
	}
}

func TestSyncBackfillFullPage(t *testing.T) {
	setTestPerPage(t, 10)
	ms := mockserver.New(t)

	// Add an issue and exactly 2 full pages of events (20 events)
	ms.AddIssues(1)
	ms.AddEvents(20, 1)

	ms.ForceBackfill = true

	client := ms.Client(t)
	rootDir := t.TempDir()

	// Initial sync
	t.Log("initial sync")
	if err := Sync(client, "owner", "repo", rootDir, nil); err != nil {
		t.Fatalf("First sync failed: %v", err)
	}
	ms.VerifyDir(t, filepath.Join(rootDir, "owner", "repo"))
	// We expect 2 pages to be fetched.
	// Page 1: 10 items, hasMore=true, returns Link to page 2.
	// Page 2: 10 items, hasMore=false, returns no Link.
	if ms.IssueEventsFetches != 2 {
		t.Fatalf("got %d issue event fetches, but expected 2 pages", ms.IssueEventsFetches)
	}

	// Second sync, no changes.
	// Page 1: Cached ETag. returns 304. Link to Page 2.
	// Page 2: ETag was NOT cached because it was a full page with no Link. Unconditional fetch.
	//         Returns 10 items, no Link.
	t.Log("sync with no changes")
	ms.IssueEventsFetches = 0
	ms.EtagFetches = 0
	if err := Sync(client, "owner", "repo", rootDir, nil); err != nil {
		t.Fatalf("Second sync failed: %v", err)
	}
	// 1 unconditional fetch (page 2), 1 etag hit (page 1)
	if ms.IssueEventsFetches != 1 || ms.EtagFetches != 1 {
		t.Fatalf("expected 1 new backfill fetch and 1 etags hits, but got %d fetches and %d etags hits", ms.IssueEventsFetches, ms.EtagFetches)
	}

	// Add a new event (total 21). This falls onto page 3.
	t.Log("sync with new event")
	ms.IssueEventsFetches = 0
	ms.EtagFetches = 0
	ms.AddEvents(1, 1) // adding one more event to issue 1
	if err := Sync(client, "owner", "repo", rootDir, nil); err != nil {
		t.Fatalf("Third sync failed: %v", err)
	}
	ms.VerifyDir(t, filepath.Join(rootDir, "owner", "repo"))

	// Page 1: ETag hit (304). Link to page 2.
	// Page 2: ETag NOT cached. Unconditional fetch. Returns 10 items. hasMore=true (end=20 < 21). Link to page 3.
	// Page 3: Unconditional fetch. Returns 1 item. hasMore=false. ETag IS cached (1 item < 10).
	if ms.IssueEventsFetches != 2 || ms.EtagFetches != 1 {
		t.Fatalf("expected 2 new backfill fetch and 1 etags hits, but got %d fetches and %d etags hits", ms.IssueEventsFetches, ms.EtagFetches)
	}

	// Fourth sync, no changes.
	// Page 1: ETag hit. Link to Page 2.
	// Page 2: ETag hit. Link to Page 3. (Because page 2 returned Link last time, we DID cache its ETag).
	// Page 3: ETag hit. No link.
	t.Log("fourth sync with no changes")
	ms.IssueEventsFetches = 0
	ms.EtagFetches = 0
	if err := Sync(client, "owner", "repo", rootDir, nil); err != nil {
		t.Fatalf("Fourth sync failed: %v", err)
	}
	if ms.IssueEventsFetches != 0 || ms.EtagFetches != 3 {
		t.Fatalf("expected 0 new backfill fetches and 3 etags hits, but got %d fetches and %d etags hits", ms.IssueEventsFetches, ms.EtagFetches)
	}
}
