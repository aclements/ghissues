// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ghsync

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// metadata represents common fields we extract from issues, comments, and events.
type metadata struct {
	ID        int64     `json:"id"`
	Number    int       `json:"number,omitempty"` // For issues
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at,omitempty"` // Events typically don't have updated_at
	IssueURL  string    `json:"issue_url,omitempty"`  // For comments

	Issue *struct { // For events
		Number int `json:"number"`
	} `json:"issue,omitempty"`
}

// issueNumber tries to determine the issue number this metadata belongs to.
func (m *metadata) issueNumber() (int, error) {
	if m.Number != 0 {
		// Issue
		return m.Number, nil
	}
	if m.Issue != nil && m.Issue.Number != 0 {
		// Event
		return m.Issue.Number, nil
	}
	if m.IssueURL != "" {
		// Issue comment
		parts := strings.Split(m.IssueURL, "/")
		if len(parts) == 0 {
			return 0, fmt.Errorf("invalid issue_url: %s", m.IssueURL)
		}
		numStr := parts[len(parts)-1]
		num, err := strconv.Atoi(numStr)
		if err != nil {
			return 0, fmt.Errorf("parsing issue number from url %s: %w", m.IssueURL, err)
		}
		return num, nil
	}
	return 0, fmt.Errorf("could not determine issue number from metadata")
}

func parseMetadata(raw json.RawMessage) (*metadata, error) {
	var meta metadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("unmarshaling metadata: %w", err)
	}
	return &meta, nil
}
