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
)

func TestSyncBasic(t *testing.T) {
	ms := newMockServer(t)

	ms.addIssues(51)
	ms.addComments(1, 1)
	ms.addComments(51, 2)
	ms.addEvents(1, 1)
	ms.addEvents(49, 3)

	client := ms.Client(t)

	rootDir := t.TempDir()

	// 1. Initial sync
	if err := Sync(client, "owner", "repo", rootDir, nil); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	ms.verifyDir(t, filepath.Join(rootDir, "owner", "repo"))

	// 2. Sync again with no changes
	statePath := filepath.Join(rootDir, "owner", "repo", "sync_state.json")
	beforeState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	if err := Sync(client, "owner", "repo", rootDir, nil); err != nil {
		t.Fatalf("Sync (no changes) failed: %v", err)
	}

	afterState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeState, afterState) {
		t.Errorf("sync_state.json changed despite no changes")
	}

	ms.verifyDir(t, filepath.Join(rootDir, "owner", "repo"))

	// 3. Add to corpus
	ms.addIssues(1)
	ms.addComments(1, 2)
	ms.addEvents(1, 2)

	// 4. Sync again and check
	if err := Sync(client, "owner", "repo", rootDir, nil); err != nil {
		t.Fatalf("Sync (with changes) failed: %v", err)
	}
	ms.verifyDir(t, filepath.Join(rootDir, "owner", "repo"))
}

func TestSyncResume(t *testing.T) {
	// Configure the server to return a non-transient error that will cause the
	// sync loop to exit after a single (new) successful request.
	ms := newMockServer(t)
	ms.testResume = true

	ms.addIssues(51)
	ms.addComments(1, 1)
	ms.addComments(51, 2)
	ms.addEvents(1, 1)
	ms.addEvents(49, 2)

	client := ms.Client(t)

	rootDir := t.TempDir()

	// Loop until sync completes without error
	tries := 0
	maxRetries := 20
	success := false
	for ; tries < maxRetries; tries++ {
		err := Sync(client, "owner", "repo", rootDir, nil)

		const maxFetchesPer = 3
		if ms.fetches > maxFetchesPer {
			// Make sure we're not just starting over from scratch each time and
			// getting a little further.
			t.Fatalf("sync made %d repeated requests; want <= %d", ms.fetches, maxFetchesPer)
		}
		if err == nil {
			success = true
			break
		}

		t.Logf("sync failed (expected): %s", err)
		// Reset failure for next iteration
		ms.failAll = false
		ms.fetches = 0
	}
	t.Logf("sync took %d tries", tries)

	if !success {
		t.Fatalf("Sync failed to complete after %d retries", maxRetries)
	}

	ms.verifyDir(t, filepath.Join(rootDir, "owner", "repo"))
}

func TestSyncUpdate(t *testing.T) {
	ms := newMockServer(t)

	ms.addIssues(1)
	ms.addComments(1, 1)

	client := ms.Client(t)

	rootDir := t.TempDir()

	// 1. Initial sync
	if err := Sync(client, "owner", "repo", rootDir, nil); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	ms.verifyDir(t, filepath.Join(rootDir, "owner", "repo"))

	// 2. Update issue and comment
	ms.Issues[0].Title = "Updated Title"
	ms.Issues[0].UpdatedAt = ms.Issues[0].UpdatedAt.Add(time.Hour)
	ms.Comments[0].Body = "Updated Comment"
	ms.Comments[0].UpdatedAt = ms.Comments[0].UpdatedAt.Add(time.Hour)

	// 3. Sync again
	if err := Sync(client, "owner", "repo", rootDir, nil); err != nil {
		t.Fatalf("Sync (with updates) failed: %v", err)
	}
	ms.verifyDir(t, filepath.Join(rootDir, "owner", "repo"))
}
