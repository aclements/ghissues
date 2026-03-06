// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ghsync

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/aclements/ghmirror/internal/github"
)

// Reporter allows the caller to receive progress updates and non-fatal warnings
// during the synchronization process.
type Reporter interface {
	// Logf is used for standard status updates.
	Logf(format string, args ...any)

	// Progress is called to signal progress on a given task.
	// Consecutive calls with the same msg generally indicate incremental progress on that task.
	Progress(msg string)

	// ProgressDone is called when a task is completed, indicating its final status.
	ProgressDone(msg, status string)
}

// syncer manages the synchronization process.
type syncer struct {
	client   *github.Client
	owner    string
	repo     string
	rootDir  string
	baseDir  string
	reporter Reporter
}

type noopReporter struct{}

func (noopReporter) Logf(format string, args ...any) {}
func (noopReporter) Progress(msg string)             {}
func (noopReporter) ProgressDone(msg, status string) {}

// Sync performs an incremental synchronization of issues, comments, and events
// for the specified repository into the provided root directory.
//
// Data is stored in a structured, chronologically sortable format using raw
// JSON from the GitHub API. The process is fully resumable; progress is saved
// after every API page to disk.
//
// Upon successful completion of the API synchronization, all changes are
// automatically committed to a Git repository initialized at the rootDir.
//
// If reporter is nil, progress updates are discarded.
func Sync(client *github.Client, owner, repo, rootDir string, reporter Reporter) error {
	if reporter == nil {
		reporter = noopReporter{}
	}
	s := &syncer{
		client:   client,
		owner:    owner,
		repo:     repo,
		rootDir:  rootDir,
		baseDir:  filepath.Join(rootDir, owner, repo),
		reporter: reporter,
	}

	madeChanges, err := s.sync()
	if err != nil {
		return err
	}

	if !madeChanges {
		s.reporter.Logf("No updates found for %s/%s.\n", owner, repo)
		return nil
	}

	s.reporter.Logf("Committing changes to Git...\n")
	if err := s.commitChanges(); err != nil {
		return fmt.Errorf("committing changes: %w", err)
	}

	return nil
}

// sync performs the incremental synchronization.
func (s *syncer) sync() (bool, error) {
	state, err := loadState(s.baseDir)
	if err != nil {
		return false, fmt.Errorf("loading state: %w", err)
	}

	if state.Version == 0 {
		state.Version = 1
	}
	if state.Version > 1 {
		return false, fmt.Errorf("state file version %d is newer than supported version 1", state.Version)
	}

	if state.IssuesNextURL == "" {
		state.IssuesNextURL = fmt.Sprintf("https://api.github.com/repos/%s/%s/issues?state=all&sort=updated&direction=asc&per_page=100", s.owner, s.repo)
		if !state.LastIssueSync.IsZero() {
			state.IssuesNextURL += "&since=" + url.QueryEscape(state.LastIssueSync.Format(time.RFC3339))
		}
	}

	if state.CommentsNextURL == "" {
		state.CommentsNextURL = fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/comments?sort=updated&direction=asc&per_page=100", s.owner, s.repo)
		if !state.LastCommentSync.IsZero() {
			state.CommentsNextURL += "&since=" + url.QueryEscape(state.LastCommentSync.Format(time.RFC3339))
		}
	}

	if state.EventsNextURL == "" {
		state.EventsNextURL = fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/events?per_page=100", s.owner, s.repo)
	}

	syncMsg := fmt.Sprintf("Syncing %s/%s", s.owner, s.repo)
	s.reporter.Progress(syncMsg)

	syncState := "failed"
	defer func() {
		if syncState != "done" {
			s.reporter.ProgressDone(syncMsg, syncState)
		}
	}()

	stateDirty := false
	var newestIssueTime, newestCommentTime, newestEventTime time.Time

	for state.IssuesNextURL != "" || state.CommentsNextURL != "" || state.EventsNextURL != "" {
		s.reporter.Progress(syncMsg)

		if state.IssuesNextURL != "" {
			// Issues are mutable (title, body, labels can change). We always
			// process all items returned by the 'since' query to capture updates.
			next, didWork, newest, err := s.fetchPage(state.IssuesNextURL, func(meta *metadata) string { return "issue.json" }, time.Time{})
			if err != nil {
				return false, fmt.Errorf("syncing issues: %w", err)
			}
			state.IssuesNextURL = next
			stateDirty = stateDirty || didWork
			if newest.After(newestIssueTime) {
				newestIssueTime = newest
			}
		}

		if state.CommentsNextURL != "" {
			// Comments are mutable (text can be edited). We always process all
			// items returned by the 'since' query to capture updates.
			next, didWork, newest, err := s.fetchPage(state.CommentsNextURL, func(meta *metadata) string {
				timeStr := meta.CreatedAt.UTC().Format(time.RFC3339)
				return fmt.Sprintf("%s-comment-%d.json", timeStr, meta.ID)
			}, time.Time{})
			if err != nil {
				return false, fmt.Errorf("syncing comments: %w", err)
			}
			state.CommentsNextURL = next
			stateDirty = stateDirty || didWork
			if newest.After(newestCommentTime) {
				newestCommentTime = newest
			}
		}

		if state.EventsNextURL != "" {
			// Events are immutable and the API does not support 'since'.
			// It returns the global firehose in descending order (newest first).
			// We paginate backward until we hit an event older than our last sync.
			next, didWork, newest, err := s.fetchPage(state.EventsNextURL, func(meta *metadata) string {
				timeStr := meta.CreatedAt.UTC().Format(time.RFC3339)
				return fmt.Sprintf("%s-event-%d.json", timeStr, meta.ID)
			}, state.LastEventSync)
			if err != nil {
				return false, fmt.Errorf("syncing events: %w", err)
			}
			state.EventsNextURL = next
			stateDirty = stateDirty || didWork
			if newest.After(newestEventTime) {
				newestEventTime = newest
			}
		}

		if stateDirty {
			if err := saveState(s.baseDir, state); err != nil {
				return false, fmt.Errorf("saving state: %w", err)
			}
		}
	}

	if stateDirty {
		// Update state for the next run using the exact timestamps from GitHub.
		// Since parameters in the GitHub API are inclusive, we don't need to
		// adjust these. The next sync will simply re-fetch the boundary items
		// and safely overwrite them due to our bytes.Equal check.
		if !newestIssueTime.IsZero() {
			state.LastIssueSync = newestIssueTime.UTC()
		}
		if !newestCommentTime.IsZero() {
			state.LastCommentSync = newestCommentTime.UTC()
		}
		if !newestEventTime.IsZero() {
			state.LastEventSync = newestEventTime.UTC()
		}

		if err := saveState(s.baseDir, state); err != nil {
			return false, fmt.Errorf("saving final state: %w", err)
		}
	}

	syncState = "done"
	s.reporter.ProgressDone(syncMsg, "done")
	return stateDirty, nil
}

func (s *syncer) fetchPage(urlStr string, nameFunc func(*metadata) string, stopTime time.Time) (string, bool, time.Time, error) {
	items, nextPage, err := s.client.DoRequestList(urlStr)
	if err != nil {
		return "", false, time.Time{}, err
	}

	madeChanges := false
	var newest time.Time
	for _, item := range items {
		meta, err := parseMetadata(item)
		if err != nil {
			return "", false, time.Time{}, fmt.Errorf("failed to parse metadata: %w", err)
		}

		if meta.UpdatedAt.After(newest) {
			newest = meta.UpdatedAt
		}
		if meta.CreatedAt.After(newest) {
			newest = meta.CreatedAt
		}

		issueNum, err := meta.issueNumber()
		if err != nil {
			continue
		}

		if !stopTime.IsZero() && !meta.CreatedAt.IsZero() && meta.CreatedAt.Before(stopTime) {
			// For immutable descending streams (events), if the event is older
			// than our last successful sync, we know we've caught up.
			return "", madeChanges, newest, nil
		}

		filename := nameFunc(meta)
		dir := filepath.Join(s.baseDir, "issues", fmt.Sprintf("%d", issueNum))
		path := filepath.Join(dir, filename)

		var prettyJSON bytes.Buffer
		if err := json.Indent(&prettyJSON, item, "", "  "); err != nil {
			prettyJSON.Write(item)
		}
		prettyJSON.WriteByte('\n')

		// Check if file already exists and has identical contents to avoid spurious state updates
		existingData, err := os.ReadFile(path)
		if err == nil && bytes.Equal(existingData, prettyJSON.Bytes()) {
			// No change to this item
			continue
		}

		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", false, time.Time{}, fmt.Errorf("creating directory %s: %w", dir, err)
		}

		tmpPath := path + ".tmp"
		if err := os.WriteFile(tmpPath, prettyJSON.Bytes(), 0644); err != nil {
			return "", false, time.Time{}, fmt.Errorf("writing temporary file %s: %w", tmpPath, err)
		}

		if err := os.Rename(tmpPath, path); err != nil {
			os.Remove(tmpPath) // Best effort cleanup
			return "", false, time.Time{}, fmt.Errorf("renaming file %s: %w", path, err)
		}

		madeChanges = true
	}

	return nextPage, madeChanges, newest, nil
}
