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
)

const stateFilename = "sync_state.json"

// state represents the synchronization state. It is saved to disk continuously
// to allow resuming interrupted syncs, and upon completion to act as the
// baseline for the next incremental sync.
type state struct {
	// Version is the schema version of this state file. Defaults to 2.
	Version int `json:"version"`

	Issues   streamState   `json:"issues"`
	Comments streamState   `json:"comments"`
	Events   streamState   `json:"events"`
	Backfill backfillState `json:"backfill,omitzero"`

	baseDir     string                  `json:"-"`
	perIssue    map[int]*IssueSyncState `json:"-"`
	dirtyIssues []int                   `json:"-"` // perIssue states that need to be saved
}

// IssueSyncState represents the per-issue synchronization state.
type IssueSyncState struct {
	ETags map[string]PageState `json:"events_pages,omitempty"`

	repoState *state `json:"-"`
	issue     int    `json:"-"`
	dirty     bool   `json:"-"`
}

// PageState represents the cached state for a single page.
type PageState struct {
	ETag    string `json:"etag"`
	NextURL string `json:"next_url,omitempty"`
}

// loadState reads the synchronization state from disk.
func loadState(baseDir string) (*state, error) {
	const version = 2

	var state state
	state.baseDir = baseDir

	statePath := filepath.Join(baseDir, stateFilename)
	data, err := os.ReadFile(statePath)
	if errors.Is(err, os.ErrNotExist) {
		state.Version = version
	} else if err != nil {
		return nil, fmt.Errorf("reading state file: %w", err)
	} else {
		if err := json.Unmarshal(data, &state); err != nil {
			return nil, fmt.Errorf("unmarshaling state: %w", err)
		}
		if state.Version != version {
			return nil, fmt.Errorf("state file version %d does not match supported version %d", state.Version, version)
		}
	}
	return &state, nil
}

// save writes the synchronization state to disk atomically.
func (s *state) save() error {
	// Ensure base directory exists
	if err := os.MkdirAll(s.baseDir, 0755); err != nil {
		return fmt.Errorf("creating base directory: %w", err)
	}
	statePath := filepath.Join(s.baseDir, stateFilename)
	if err := writeJSON(s, statePath); err != nil {
		return err
	}

	// Write dirty per-issue state
	for _, dirty := range s.dirtyIssues {
		dir := filepath.Join(s.baseDir, "issues", fmt.Sprintf("%d", dirty))
		statePath := filepath.Join(dir, stateFilename)
		if err := writeJSON(s.perIssue[dirty], statePath); err != nil {
			return err
		}
		s.perIssue[dirty].dirty = false
	}
	s.dirtyIssues = s.dirtyIssues[:0]

	return nil
}

func writeJSON(val any, path string) error {
	data, err := json.MarshalIndent(val, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}
	data = append(data, '\n')

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("writing temporary JSON file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath) // Best effort cleanup
		return fmt.Errorf("renaming JSON file: %w", err)
	}

	return nil
}

func (s *state) getIssueState(issue int) (*IssueSyncState, error) {
	if s.perIssue == nil {
		s.perIssue = make(map[int]*IssueSyncState)
	}
	is, _ := s.perIssue[issue]
	if is == nil {
		// Load from disk
		statePath := filepath.Join(s.baseDir, "issues", fmt.Sprintf("%d", issue), stateFilename)
		data, err := os.ReadFile(statePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				is = &IssueSyncState{}
			} else {
				return nil, fmt.Errorf("reading issue state file: %w", err)
			}
		} else {
			var state IssueSyncState
			if err := json.Unmarshal(data, &state); err != nil {
				return nil, fmt.Errorf("unmarshaling issue state: %w", err)
			}
			is = &state
		}
		is.repoState = s
		is.issue = issue
		s.perIssue[issue] = is
	}
	return is, nil
}

type etagCache interface {
	// getETag returns the cached ETag and "next" link URL for the given URL in
	// the page cache, or "".
	getETag(url string) (etag, link string)
	// putETag records an ETag to the cache.
	putETag(url string, etag, link string)
}

func (s *IssueSyncState) getETag(url string) (etag, link string) {
	if page, ok := s.ETags[url]; ok {
		return page.ETag, page.NextURL
	}
	return "", ""
}

func (s *IssueSyncState) putETag(url string, etag, link string) {
	page := PageState{etag, link}
	if s.ETags[url] != page {
		if s.ETags == nil {
			s.ETags = make(map[string]PageState)
		}
		s.ETags[url] = page
		if !s.dirty {
			s.dirty = true
			s.repoState.dirtyIssues = append(s.repoState.dirtyIssues, s.issue)
		}
	}
}
