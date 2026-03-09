// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ghsync

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aclements/ghissues/internal/github"
)

// streamState represents the persistent state of a paginated API stream.
type streamState struct {
	// NextURL is the URL of the next page to fetch. If empty, the stream
	// may be initialized. When a stream is truly exhausted, this is empty.
	NextURL string `json:"next_url,omitempty"`

	// Newest is the latest timestamp (CreatedAt or UpdatedAt) observed in this
	// stream so far. It acts as the "since" query for the next incremental sync.
	Newest time.Time `json:"newest,omitzero"`

	// StopTime is the timestamp at which to stop fetching for descending
	// streams. If non-zero, fetching stops when encountering an item older than this.
	StopTime time.Time `json:"stop_time,omitzero"`
}

type pageStream struct {
	// client is the GitHub API client used to fetch pages.
	client *github.Client

	// madeChange is true if any item in this stream was newly created or
	// updated on disk during this sync pass.
	madeChange bool

	// pathFunc returns the full filesystem path where a given item should
	// be stored. If it returns an error, the item is skipped.
	pathFunc func(meta *metadata) (string, error)

	// initURL is called to synthesize a starting URL if nextURL is empty. It is
	// passed newest, the timestamp of the newest item seen so far. When the
	// stream is truly exhausted, fetchNext sets this to nil.
	initURL func(since time.Time) string
}

func (ps *pageStream) active(st *streamState) bool {
	return !(st.NextURL == "" && ps.initURL == nil)
}

func (ps *pageStream) done(st *streamState) {
	st.NextURL = ""
	ps.initURL = nil
}

// fetchNext retrieves a single page from the GitHub API and writes the items
// to disk. It updates st.NextURL, st.Newest, and ps.madeChange.
func (ps *pageStream) fetchNext(st *streamState) error {
	if st.NextURL == "" {
		if ps.initURL == nil {
			return nil
		}
		st.NextURL = ps.initURL(st.Newest)
	}

	items, nextPage, err := ps.client.DoRequestList(st.NextURL)
	if err != nil {
		return err
	}

	if len(items) == 0 {
		ps.done(st)
		return nil
	}

	for _, item := range items {
		meta, err := parseMetadata(item)
		if err != nil {
			return fmt.Errorf("failed to parse metadata: %w", err)
		}

		if meta.UpdatedAt.After(st.Newest) {
			st.Newest = meta.UpdatedAt
		}
		if meta.CreatedAt.After(st.Newest) {
			st.Newest = meta.CreatedAt
		}

		if !st.StopTime.IsZero() && !meta.CreatedAt.IsZero() && meta.CreatedAt.Before(st.StopTime) {
			// For immutable descending streams (events), if the event is older
			// than our last successful sync, we know we've caught up.
			ps.done(st)
			// We cleared the resumption URL, so the next sync will start back
			// at the beginning of the descending stream. Record where it should
			// stop.
			st.StopTime = st.Newest
			return nil
		}

		path, err := ps.pathFunc(meta)
		if err != nil {
			// If we can't determine the path (e.g., missing issue number),
			// skip this item.
			return err
		}
		dir := filepath.Dir(path)

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
			return fmt.Errorf("creating directory %s: %w", dir, err)
		}

		tmpPath := path + ".tmp"
		if err := os.WriteFile(tmpPath, prettyJSON.Bytes(), 0644); err != nil {
			return fmt.Errorf("writing temporary file %s: %w", tmpPath, err)
		}

		if err := os.Rename(tmpPath, path); err != nil {
			os.Remove(tmpPath) // Best effort cleanup
			return fmt.Errorf("renaming file %s: %w", path, err)
		}

		ps.madeChange = true
	}

	if nextPage != "" {
		st.NextURL = nextPage
	} else if ps.initURL != nil {
		// If we run out of pages, try starting a fresh sequence from the newest
		// timestamp we've seen. This allows us to transparently bypass GitHub's
		// 300-page limit for repository-wide lists.
		fresh := ps.initURL(st.Newest)
		if st.NextURL == fresh {
			// We already tried this exact URL and got no next page,
			// which means we've reached the true end of the stream.
			ps.done(st)
		} else {
			st.NextURL = fresh
		}
	} else {
		// No next page and no way to synthesize a fresh start URL.
		ps.done(st)
	}

	return nil
}
