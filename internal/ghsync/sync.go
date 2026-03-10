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

	state *state

	madeChanges bool
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

	state, err := loadState(s.baseDir)
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}
	s.state = state

	err = s.sync()
	if err != nil {
		return err
	}

	if !s.madeChanges {
		s.reporter.Logf("No updates found for %s/%s.\n", owner, repo)
		return nil
	}

	s.reporter.Logf("Committing changes to Git...\n")
	if err := s.commitChanges(); err != nil {
		return fmt.Errorf("committing changes: %w", err)
	}

	return nil
}

type syncStream[State any] interface {
	active(*State) bool
	fetchNext(*State) (bool, error)
}

func syncOneStream[State any](s *syncer, name string, stream syncStream[State], streamState *State) error {
	if !stream.active(streamState) {
		return nil
	}

	syncMsg := fmt.Sprintf("Syncing %s/%s %s", s.owner, s.repo, name)
	s.reporter.Progress(syncMsg)

	syncStatus := "failed"
	defer func() {
		if syncStatus != "done" {
			s.reporter.ProgressDone(syncMsg, syncStatus)
		}
	}()

	for stream.active(streamState) {
		s.reporter.Progress(syncMsg)

		madeChange, err := stream.fetchNext(streamState)
		s.madeChanges = s.madeChanges || madeChange
		err2 := s.state.save()
		if err != nil {
			return fmt.Errorf("syncing %s: %w", name, err)
		}
		if err2 != nil {
			return fmt.Errorf("saving %s state: %w", name, err2)
		}
	}

	syncStatus = "done"
	s.reporter.ProgressDone(syncMsg, syncStatus)
	return nil
}

// sync performs the incremental synchronization.
func (s *syncer) sync() error {
	state := s.state

	// Sync issues
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
	if err := syncOneStream(s, "issues", issuesStream, &state.Issues); err != nil {
		return err
	}

	// Sync comments
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
	if err := syncOneStream(s, "comments", commentsStream, &state.Comments); err != nil {
		return err
	}

	// Sync events
	if state.Events.NextURL == "" {
		// The events stream does not support restarting via since. If we have no next URL,
		// initialize it to the first page. We use state.Events.StopTime to stop once we
		// reach events we've already processed.
		state.Events.NextURL = fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/events?per_page=100", s.owner, s.repo)
	}
	eventsStream := &pageStream{
		client:       s.client,
		isDescending: true,
		pathFunc: func(meta *metadata) (string, error) {
			if meta.Issue == nil {
				// Sometimes events don't have an issue. I can't find any
				// documented reason for this, and it doesn't seem correlated
				// with the event type.
				return "", nil
			}
			issueNum, err := meta.issueNumber()
			if err != nil {
				return "", err
			}
			timeStr := meta.CreatedAt.UTC().Format(time.RFC3339)
			return filepath.Join(s.baseDir, "issues", fmt.Sprintf("%d", issueNum), fmt.Sprintf("%s-event-%d.json", timeStr, meta.ID)), nil
		},
	}
	if err := syncOneStream(s, "events", eventsStream, &state.Events); err != nil {
		return err
	}

	// If we exhausted the event stream, start the backfill sync
	backfill := &backfillStream{
		client:    s.client,
		owner:     s.owner,
		repo:      s.repo,
		baseDir:   s.baseDir,
		repoState: s.state,
	}
	if !eventsStream.hitStop && !backfill.active(&state.Backfill) {
		s.reporter.Logf("reached end of repo events stream; starting per-issue event backfill")
		if err := backfill.start(&state.Backfill); err != nil {
			return fmt.Errorf("starting backfill: %w", err)
		}
	}

	// Sync events backfill
	if err := syncOneStream(s, "events backfill", backfill, &state.Backfill); err != nil {
		return err
	}

	return nil
}
