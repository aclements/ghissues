// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ghsync

import (
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	"github.com/aclements/ghissues/internal/github"
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
		// Initialize to version 2
		state.Version = 2
	}
	if state.Version != 2 {
		return false, fmt.Errorf("state file version %d is not supported version 2", state.Version)
	}

	issuesStream := &pageStream{
		client: s.client,
		pathFunc: func(meta *metadata) (string, error) {
			issueNum, err := meta.issueNumber()
			if err != nil {
				return "", err
			}
			return filepath.Join(s.baseDir, "issues", fmt.Sprintf("%d", issueNum), "issue.json"), nil
		},
		initURL: func(since time.Time) string {
			urlStr := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues?state=all&sort=updated&direction=asc&per_page=100", s.owner, s.repo)
			if !since.IsZero() {
				urlStr += "&since=" + url.QueryEscape(since.Format(time.RFC3339))
			}
			return urlStr
		},
	}

	commentsStream := &pageStream{
		client: s.client,
		pathFunc: func(meta *metadata) (string, error) {
			issueNum, err := meta.issueNumber()
			if err != nil {
				return "", err
			}
			timeStr := meta.CreatedAt.UTC().Format(time.RFC3339)
			return filepath.Join(s.baseDir, "issues", fmt.Sprintf("%d", issueNum), fmt.Sprintf("%s-comment-%d.json", timeStr, meta.ID)), nil
		},
		initURL: func(since time.Time) string {
			urlStr := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/comments?sort=updated&direction=asc&per_page=100", s.owner, s.repo)
			if !since.IsZero() {
				urlStr += "&since=" + url.QueryEscape(since.Format(time.RFC3339))
			}
			return urlStr
		},
	}

	if state.Events.NextURL == "" && state.Events.Newest.IsZero() {
		// The events stream does not support restarting via since. If we have no next URL
		// and we haven't seen any newest events (implying this is a completely fresh stream),
		// initialize it.
		state.Events.NextURL = fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/events?per_page=100", s.owner, s.repo)
	}
	eventsStream := &pageStream{
		client: s.client,
		pathFunc: func(meta *metadata) (string, error) {
			issueNum, err := meta.issueNumber()
			if err != nil {
				return "", err
			}
			timeStr := meta.CreatedAt.UTC().Format(time.RFC3339)
			return filepath.Join(s.baseDir, "issues", fmt.Sprintf("%d", issueNum), fmt.Sprintf("%s-event-%d.json", timeStr, meta.ID)), nil
		},
	}

	anyChanges := func() bool {
		return issuesStream.madeChange || commentsStream.madeChange || eventsStream.madeChange
	}

	syncMsg := fmt.Sprintf("Syncing %s/%s", s.owner, s.repo)
	s.reporter.Progress(syncMsg)

	syncState := "failed"
	defer func() {
		if syncState != "done" {
			s.reporter.ProgressDone(syncMsg, syncState)
		}
	}()

	for issuesStream.active(&state.Issues) || commentsStream.active(&state.Comments) || eventsStream.active(&state.Events) {
		s.reporter.Progress(syncMsg)

		if err := issuesStream.fetchNext(&state.Issues); err != nil {
			return false, fmt.Errorf("syncing issues: %w", err)
		}

		if err := commentsStream.fetchNext(&state.Comments); err != nil {
			return false, fmt.Errorf("syncing comments: %w", err)
		}

		if err := eventsStream.fetchNext(&state.Events); err != nil {
			return false, fmt.Errorf("syncing events: %w", err)
		}

		if anyChanges() {
			if err := saveState(s.baseDir, state); err != nil {
				return false, fmt.Errorf("saving state: %w", err)
			}
		}
	}

	if anyChanges() {
		// Prepare StopTime for the next run.
		state.Events.StopTime = state.Events.Newest

		if err := saveState(s.baseDir, state); err != nil {
			return false, fmt.Errorf("saving final state: %w", err)
		}
	}

	syncState = "done"
	s.reporter.ProgressDone(syncMsg, "done")
	return anyChanges(), nil
}
