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
	"strconv"
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
	// streams. If isDescending is set, fetching stops when encountering an
	// item older than this.
	StopTime time.Time `json:"stop_time,omitzero"`

	// cache provides access to cache metadata state.
	cache etagCache `json:"-"`
}

// pageStream manages fetching pages from a GitHub API stream.
//
// There are three categories of data streams:
//
// - Ascending streams (oldest first) with "since" filtering. For these, we
// start the stream at the latest timestamp we've seen ([streamState.Newest])
// and fetch as many pages from the stream as we can. pageStream updates Newest
// as it goes, and at the end of the page stream, restarts at the new Newest.
//
// - Ascending streams (oldest first) with no time filtering. For these, we
// always have to fetch the whole stream. ETag filtering is effective for this
// type of stream, but generally we try to avoid using these streams.
//
// - Descending streams (newest first) with no filtering. For these, we start at
// the beginning of the stream (we have no other choice) and fetch pages until
// we see an object who's timestamp is >= [streamState.StopTime], or we reach
// the end of the stream. Only at that point do we update [stream.StopTime] to
// match [stream.Newest]. This has the effect of breaking the stream into
// "segments", where we have to fetch a complete segment before we start back at
// the beginning.
type pageStream struct {
	// client is the GitHub API client used to fetch pages.
	client *github.Client

	// isDescending indicates that this is a descending stream, for which we
	// should use and update StopTime.
	isDescending bool

	// pathFunc returns the full filesystem path where a given item should
	// be stored. If it returns "", the item is skipped.
	pathFunc func(meta *metadata) (string, error)

	// initURL is called to synthesize a starting URL if nextURL is empty. It is
	// passed newest, the timestamp of the newest item seen so far. When the
	// stream is truly exhausted, fetchNext sets this to nil.
	initURL func(since time.Time) string

	// hitStop is set to true by fetchNext if the stream terminated early
	// because it encountered an item older than the streamState's StopTime.
	hitStop bool
}

func (ps *pageStream) active(st *streamState) bool {
	return !(st.NextURL == "" && ps.initURL == nil)
}

func (ps *pageStream) done(st *streamState) {
	st.NextURL = ""
	ps.initURL = nil

	if ps.isDescending {
		// This is a descending stream. We've caught up with our last "complete"
		// sync, so the next sync can start back at the (new) beginning of the
		// stream until it reaches where THIS sync started.
		st.StopTime = st.Newest
	}
}

// fetchNext retrieves a single page from the GitHub API and writes the items to
// disk. It updates st.NextURL and st.Newest. It returns whether any item in
// this stream was newly created or updated on disk during this sync pass.
func (ps *pageStream) fetchNext(st *streamState) (bool, error) {
	if st.NextURL == "" {
		if ps.initURL == nil {
			return false, nil
		}
		st.NextURL = ps.initURL(st.Newest)
	}

	reqURL := st.NextURL
	var opts github.RequestOptions
	var cachedLink string
	if st.cache != nil {
		opts.ETag, cachedLink = st.cache.getETag(reqURL)
	}

	items, resp, err := ps.client.DoRequestList(reqURL, &opts)
	if err != nil {
		return false, err
	}

	if resp.NotModified {
		if cachedLink == "" {
			ps.done(st)
		} else {
			st.NextURL = cachedLink
		}
		return false, nil
	}

	madeChange := false
	for i, item := range items {
		meta, err := parseMetadata(item)
		if err != nil {
			return madeChange, fmt.Errorf("failed to parse metadata for item %d of %s: %w", i, reqURL, err)
		}

		if meta.UpdatedAt.After(st.Newest) {
			st.Newest = meta.UpdatedAt
		}
		if meta.CreatedAt.After(st.Newest) {
			st.Newest = meta.CreatedAt
		}

		if ps.isDescending && !meta.CreatedAt.IsZero() && meta.CreatedAt.Before(st.StopTime) {
			// For immutable descending streams (events), if the event is older
			// than our last successful sync, we know we've caught up.
			ps.hitStop = true
			ps.done(st)
			return madeChange, nil
		}

		path, err := ps.pathFunc(meta)
		if err != nil {
			// If we can't determine the path (e.g., missing issue number), fail.
			return madeChange, fmt.Errorf("constructing local path for item %d of %s: %w", i, reqURL, err)
		}
		if path == "" {
			// Skip
			continue
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
			return madeChange, fmt.Errorf("creating directory %s: %w", dir, err)
		}

		tmpPath := path + ".tmp"
		if err := os.WriteFile(tmpPath, prettyJSON.Bytes(), 0644); err != nil {
			return madeChange, fmt.Errorf("writing temporary file %s: %w", tmpPath, err)
		}

		if err := os.Rename(tmpPath, path); err != nil {
			os.Remove(tmpPath) // Best effort cleanup
			return madeChange, fmt.Errorf("renaming file %s: %w", path, err)
		}
		madeChange = true
	}

	// Now that we've written all items, update the cached ETag.
	if st.cache != nil {
		// If this is the last page and it might be full, do not cache its ETag.
		// ETags generally cover the response body but not headers like Link.
		// If the last page has exactly per_page items, and more items are
		// added later, fetching it again with the same ETag would return a
		// 304 Not Modified. This would cause us to miss the new Link header
		// pointing to the next page. By skipping the ETag cache here, we
		// force a re-fetch to ensure we don't miss new pages.
		cacheETag := true
		if resp.NextURL == "" && len(items) > 0 {
			pageSize := -1
			if u, err := url.Parse(reqURL); err == nil {
				if q := u.Query().Get("per_page"); q != "" {
					if p, err := strconv.Atoi(q); err == nil {
						pageSize = p
					}
				}
			}
			// If we cannot determine the page size (pageSize < 0),
			// conservatively assume it might be full.
			if pageSize < 0 || len(items) >= pageSize {
				cacheETag = false
			}
		}

		if cacheETag {
			st.cache.putETag(reqURL, resp.ETag, resp.NextURL)
		} else {
			st.cache.putETag(reqURL, "", "")
		}
	}

	if len(items) == 0 {
		ps.done(st)
		return false, nil
	}

	if resp.NextURL != "" {
		st.NextURL = resp.NextURL
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

	return madeChange, nil
}
