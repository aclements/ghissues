// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ghsync

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/aclements/ghissues/internal/github"
)

// backfillState tracks the state of the historical event backfill process.
type backfillState struct {
	// CurrentIssue is the issue number currently being backfilled.
	// If 0, backfilling is not active or has not started.
	CurrentIssue int `json:"current_issue,omitempty"`

	// Stream tracks the pagination state within the events of CurrentIssue.
	Stream streamState `json:"stream"`
}

type backfillStream struct {
	client  *github.Client
	owner   string
	repo    string
	baseDir string

	pendingIssues []int // Not including the current issue.
	repoState     *state
	ps            *pageStream
}

// start initializes a new backfill process.
func (bs *backfillStream) start(st *backfillState) error {
	*st = backfillState{}
	return bs.nextIssue(st)
}

func (bs *backfillStream) nextIssue(st *backfillState) error {
	if len(bs.pendingIssues) == 0 {
		// Either we haven't loaded the pending issues, or we've exhausted the
		// list and should check the file system again.
		if err := bs.populatePendingIssues(st.CurrentIssue); err != nil {
			return err
		}
		if len(bs.pendingIssues) == 0 {
			// We're done.
			*st = backfillState{}
			return nil
		}
	}

	// Pop the next issue off the pending queue.
	issueNum := bs.pendingIssues[0]
	bs.pendingIssues = bs.pendingIssues[1:]
	st.CurrentIssue = issueNum

	// Reset to the page stream of the new issue.
	etagCache, err := bs.repoState.getIssueState(issueNum)
	if err != nil {
		return err
	}
	st.Stream = streamState{
		NextURL: fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d/events?per_page=100", bs.owner, bs.repo, issueNum),
		cache:   etagCache,
	}
	return nil
}

func (bs *backfillStream) populatePendingIssues(current int) error {
	issuesDir := filepath.Join(bs.baseDir, "issues")
	entries, err := os.ReadDir(issuesDir)
	if err != nil {
		if os.IsNotExist(err) {
			bs.pendingIssues = nil
			return nil
		}
		return fmt.Errorf("reading issues directory: %w", err)
	}

	var issues []int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		var num int
		if _, err := fmt.Sscanf(entry.Name(), "%d", &num); err == nil {
			if num > current {
				issues = append(issues, num)
			}
		}
	}

	slices.Sort(issues)
	bs.pendingIssues = issues
	return nil
}

func (bs *backfillStream) active(st *backfillState) bool {
	return st.CurrentIssue != 0
}

// fetch retrieves the next page of events for the current backfill issue.
func (bs *backfillStream) fetchNext(st *backfillState) (bool, error) {
	if !bs.active(st) {
		return false, nil
	}

	if bs.ps == nil {
		// Configure the page stream on the first fetch.
		bs.ps = &pageStream{
			client: bs.client,
			pathFunc: func(meta *metadata) (string, error) {
				metaIssue, err := meta.issueNumber()
				if err == nil && metaIssue != st.CurrentIssue {
					return "", fmt.Errorf("event belongs to issue %d, expected %d", metaIssue, st.CurrentIssue)
				}
				timeStr := meta.CreatedAt.UTC().Format(time.RFC3339)
				return filepath.Join(bs.baseDir, "issues", fmt.Sprintf("%d", st.CurrentIssue), fmt.Sprintf("%s-event-%d.json", timeStr, meta.ID)), nil
			},
		}
	}

	deleted := false
	madeChange, err := bs.ps.fetchNext(&st.Stream)
	if err != nil {
		if apiErr, ok := err.(*github.APIError); ok && apiErr.StatusCode == 404 {
			// Deleted issue, move on
			deleted = true
		} else {
			return madeChange, err
		}
	}
	if deleted || !bs.ps.active(&st.Stream) {
		// We've reached the end of this issue's stream.
		if err := bs.nextIssue(st); err != nil {
			return madeChange, err
		}
		return madeChange, nil
	}

	return madeChange, nil
}
