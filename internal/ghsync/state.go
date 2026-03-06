// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ghsync

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const stateFilename = "sync_state.json"

// state represents the synchronization state. It is saved to disk continuously
// to allow resuming interrupted syncs, and upon completion to act as the
// baseline for the next incremental sync.
type state struct {
	// Version is the schema version of this state file. Defaults to 1.
	Version int `json:"version"`

	// These NextURL fields represent the active paginator for each stream.
	// They are populated at the start of a sync and updated after every page
	// is successfully processed. When a sync completes fully, these are empty.
	// If a sync is interrupted, the next run will resume from these URLs.
	IssuesNextURL   string `json:"issues_next_url,omitempty"`
	CommentsNextURL string `json:"comments_next_url,omitempty"`
	EventsNextURL   string `json:"events_next_url,omitempty"`

	// These Sync fields record the timestamp of the last *fully completed*
	// sync pass. They are used to generate the "since" queries for the next
	// incremental sync run (or as the stopping condition for the Events stream).
	// They are ONLY updated at the very end of a successful sync pass.
	LastIssueSync   time.Time `json:"last_issue_sync,omitzero"`
	LastCommentSync time.Time `json:"last_comment_sync,omitzero"`
	LastEventSync   time.Time `json:"last_event_sync,omitzero"`
}

// loadState reads the synchronization state from disk.
func loadState(baseDir string) (*state, error) {
	statePath := filepath.Join(baseDir, stateFilename)
	data, err := os.ReadFile(statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &state{}, nil
		}
		return nil, fmt.Errorf("reading state file: %w", err)
	}

	var state state
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("unmarshaling state: %w", err)
	}
	return &state, nil
}

// saveState writes the synchronization state to disk atomically.
func saveState(baseDir string, state *state) error {
	statePath := filepath.Join(baseDir, stateFilename)

	// Ensure base directory exists
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return fmt.Errorf("creating base directory: %w", err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	tmpPath := statePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("writing temporary state file: %w", err)
	}

	if err := os.Rename(tmpPath, statePath); err != nil {
		os.Remove(tmpPath) // Best effort cleanup
		return fmt.Errorf("renaming state file: %w", err)
	}

	return nil
}
