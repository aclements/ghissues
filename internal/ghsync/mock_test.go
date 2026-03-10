// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ghsync

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aclements/ghissues/internal/github"
)

type mockIssue struct {
	ID        int64     `json:"id"`
	Number    int       `json:"number"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	State     string    `json:"state"`
	Title     string    `json:"title"`
}

type mockComment struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	IssueURL  string    `json:"issue_url"`
	Body      string    `json:"body"`
}

type mockEvent struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Issue     *struct {
		Number int `json:"number"`
	} `json:"issue"`
	Event string `json:"event"`
}

type mockServer struct {
	t *testing.T

	Issues   []mockIssue
	Comments []mockComment
	Events   []mockEvent

	// nextID is for generating object IDs
	nextID int64

	// fetches counts successful fetches, not including NotModified resposes.
	// The caller is allowed to reset this.
	fetches int
	// etagFetches counts NotModified fetches.
	etagFetches int
	// issueEventsFetches is the number of successful issue events fetches.
	issueEventsFetches int

	// maxFetches is a limit on fetches to prevent infinite loops.
	maxFetches int

	// forceBackfill causes the repo-wide issue events endpoint to return an
	// empty list, forcing a backfill from the per-issue events endpoints.
	forceBackfill bool

	// testResume enables "resumption testing" mode, where the first time the
	// server gets a request for a new URL, it will set failAll to enter failure
	// mode, which causes it to respond to this and all further requests with
	// "Bad Request". The caller can reset failAll and retry, which will allow a
	// sequent request for the previously failed URL to succeed.
	testResume bool
	failAll    bool
	seenURLs   map[string]bool

	mux *http.ServeMux
}

func newMockServer(t *testing.T) *mockServer {
	s := &mockServer{
		t:          t,
		seenURLs:   make(map[string]bool),
		nextID:     1,
		maxFetches: 200,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/comments", func(w http.ResponseWriter, r *http.Request) {
		params, err := s.getParams(r, true)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, hasMore := filterAndPage(s.Comments, params, func(c mockComment) time.Time { return c.UpdatedAt })
		s.writeResponse(w, r, resp, hasMore, params.page)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/events", func(w http.ResponseWriter, r *http.Request) {
		params, err := s.getParams(r, false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		params.direction = "desc"
		events := s.Events
		if s.forceBackfill {
			events = nil
		}
		resp, hasMore := filterAndPage(events, params, func(e mockEvent) time.Time { return e.CreatedAt })
		s.writeResponse(w, r, resp, hasMore, params.page)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{issueNum}/events", func(w http.ResponseWriter, r *http.Request) {
		params, err := s.getParams(r, false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		params.direction = "asc"
		issueNum, err := strconv.Atoi(r.PathValue("issueNum"))
		if err != nil {
			http.Error(w, "bad issue number", http.StatusBadRequest)
			return
		}
		var issueEvents []mockEvent
		for _, e := range s.Events {
			if e.Issue != nil && e.Issue.Number == issueNum {
				issueEvents = append(issueEvents, e)
			}
		}
		resp, hasMore := filterAndPage(issueEvents, params, func(e mockEvent) time.Time { return e.CreatedAt })
		if s.writeResponse(w, r, resp, hasMore, params.page) {
			// Only increment on a full fetch
			s.issueEventsFetches++
		}
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		params, err := s.getParams(r, true)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, hasMore := filterAndPage(s.Issues, params, func(i mockIssue) time.Time { return i.UpdatedAt })
		s.writeResponse(w, r, resp, hasMore, params.page)
	})
	s.mux = mux
	return s
}

func (s *mockServer) addIssues(n int) {
	for range n {
		t := time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(s.nextID) * time.Hour)
		s.Issues = append(s.Issues, mockIssue{
			ID:        s.nextID,
			Number:    int(s.nextID),
			CreatedAt: t,
			UpdatedAt: t,
			State:     "open",
			Title:     fmt.Sprintf("Issue %d", s.nextID),
		})
		s.nextID++
	}
}

func (s *mockServer) addComments(n int, issueNum int) {
	for range n {
		t := time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(s.nextID) * time.Hour)
		s.Comments = append(s.Comments, mockComment{
			ID:        s.nextID,
			CreatedAt: t,
			UpdatedAt: t,
			IssueURL:  fmt.Sprintf("https://api.github.com/repos/owner/repo/issues/%d", issueNum),
			Body:      fmt.Sprintf("Comment %d", s.nextID),
		})
		s.nextID++
	}
}

func (s *mockServer) addEvents(n int, issueNum int) {
	for range n {
		t := time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(s.nextID) * time.Hour)
		s.Events = append(s.Events, mockEvent{
			ID:        s.nextID,
			CreatedAt: t,
			Issue: &struct {
				Number int `json:"number"`
			}{Number: issueNum},
			Event: "subscribed",
		})
		s.nextID++
	}
}

func (s *mockServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.t.Logf("mock serving %s", r.URL.String())
	if s.testResume {
		if s.failAll {
			s.t.Errorf("  request received after injected failure")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !s.seenURLs[r.URL.String()] {
			s.t.Logf("  injecting failure response")
			// Allow it on the next round.
			s.seenURLs[r.URL.String()] = true
			s.failAll = true
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}

	s.mux.ServeHTTP(w, r)
}

type pageParams struct {
	page      int
	perPage   int
	since     time.Time
	direction string
}

func (s *mockServer) getParams(r *http.Request, canSince bool) (pageParams, error) {
	var err error

	pageStr := r.URL.Query().Get("page")
	page := 1
	if pageStr != "" {
		if _, err := fmt.Sscanf(pageStr, "%d", &page); err != nil || page < 1 {
			return pageParams{}, fmt.Errorf("bad page %q", pageStr)
		}
	}

	perPageStr := r.URL.Query().Get("per_page")
	perPage := 10
	if perPageStr != "" {
		if _, err := fmt.Sscanf(perPageStr, "%d", &perPage); err != nil || perPage < 1 {
			return pageParams{}, fmt.Errorf("bad per_page %q", perPageStr)
		}
	}

	sinceStr := r.URL.Query().Get("since")
	var since time.Time
	if sinceStr != "" {
		if !canSince {
			return pageParams{}, fmt.Errorf("unsupported 'since' parameter")
		}
		since, err = time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			return pageParams{}, fmt.Errorf("bad since %q", pageStr)
		}
	}

	direction := r.URL.Query().Get("direction")
	if direction != "" && !canSince {
		return pageParams{}, fmt.Errorf("unsupported 'direction' parameter")
	}
	switch direction {
	case "":
		direction = "desc" // Default
	case "asc", "desc":
		// Ok
	default:
		return pageParams{}, fmt.Errorf("bad direction %q", direction)
	}

	return pageParams{page: page, perPage: perPage, since: since, direction: direction}, nil
}

func (s *mockServer) writeResponse(w http.ResponseWriter, r *http.Request, resp any, hasMore bool, page int) bool {
	if s.fetches >= s.maxFetches {
		s.t.Errorf("max fetch limit (%d) reached; infinite loop?", s.maxFetches)
		w.WriteHeader(http.StatusForbidden)
		return false
	}

	body, err := json.Marshal(resp)
	if err != nil {
		s.t.Fatalf("failed to marshal response: %v", err)
	}
	etag := fmt.Sprintf(`"%x"`, sha256.Sum256(body))

	if r.Header.Get("If-None-Match") == etag {
		s.t.Log("  matched ETag")
		w.WriteHeader(http.StatusNotModified)
		s.etagFetches++
		return false
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", etag)
	if hasMore {
		nextURL := *r.URL
		nextURL.Scheme = "http"
		if r.TLS != nil {
			nextURL.Scheme = "https"
		}
		nextURL.Host = r.Host
		if r.URL.Host != "" {
			nextURL.Host = r.URL.Host
		}
		q := nextURL.Query()
		q.Set("page", fmt.Sprintf("%d", page+1))
		nextURL.RawQuery = q.Encode()
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, nextURL.String()))
	}
	w.WriteHeader(http.StatusOK)
	w.Write(body)
	s.fetches++
	return true
}

func filterAndPage[T any](data []T, params pageParams, timestamp func(T) time.Time) ([]T, bool) {
	// Restore sort of backing data, if necessary.
	if timestamp != nil {
		dir := 1
		if params.direction == "desc" {
			dir = -1
		}
		cmp := func(a, b T) int { return dir * timestamp(a).Compare(timestamp(b)) }
		if !slices.IsSortedFunc(data, cmp) {
			slices.SortFunc(data, cmp)
		}
	}

	// Filter by "since".
	var filtered []T
	if params.since.IsZero() || timestamp == nil {
		filtered = data
	} else {
		i, _ := slices.BinarySearchFunc(data, params.since, func(d T, key time.Time) int {
			return timestamp(d).Compare(key)
		})
		filtered = data[i:]
	}

	// Get page.
	start := min((params.page-1)*params.perPage, len(filtered))
	end := min(start+params.perPage, len(filtered))
	return filtered[start:end], end < len(filtered)
}

func (s *mockServer) verifyDir(t *testing.T, dir string) {
	t.Helper()

	expectedFiles := make(map[string]any)

	// Build expected files map
	for _, issue := range s.Issues {
		path := filepath.ToSlash(filepath.Join("issues", fmt.Sprintf("%d", issue.Number), "issue.json"))
		expectedFiles[path] = issue
	}

	for _, comment := range s.Comments {
		parts := strings.Split(comment.IssueURL, "/")
		issueNum := parts[len(parts)-1]
		timeStr := comment.CreatedAt.UTC().Format(time.RFC3339)
		path := filepath.ToSlash(filepath.Join("issues", issueNum, fmt.Sprintf("%s-comment-%d.json", timeStr, comment.ID)))
		expectedFiles[path] = comment
	}

	for _, event := range s.Events {
		issueNum := fmt.Sprintf("%d", event.Issue.Number)
		timeStr := event.CreatedAt.UTC().Format(time.RFC3339)
		path := filepath.ToSlash(filepath.Join("issues", issueNum, fmt.Sprintf("%s-event-%d.json", timeStr, event.ID)))
		expectedFiles[path] = event
	}

	// Normalize expected values by round-tripping through JSON
	for path, expected := range expectedFiles {
		expB, err := json.Marshal(expected)
		if err != nil {
			t.Fatalf("Failed to marshal expected object for %s: %v", path, err)
		}
		var expectedNorm any
		if err := json.Unmarshal(expB, &expectedNorm); err != nil {
			t.Fatalf("Failed to re-parse expected object for %s: %v", path, err)
		}
		expectedFiles[path] = expectedNorm
	}

	actualFiles := make(map[string]any)

	// Walk directory to build actual files map
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}

		if filepath.Base(relPath) == "sync_state.json" {
			return nil
		}

		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		var actual any
		if err := json.Unmarshal(b, &actual); err != nil {
			return fmt.Errorf("failed to parse %s: %v", path, err)
		}

		actualFiles[filepath.ToSlash(relPath)] = actual
		return nil
	})
	if err != nil {
		t.Fatalf("Failed to walk directory %s: %v", dir, err)
	}

	// Compare keys and values
	for path, expected := range expectedFiles {
		actual, ok := actualFiles[path]
		if !ok {
			t.Errorf("Missing expected file: %s", path)
			continue
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Errorf("Content mismatch in %s:\nGot:  %+v\nWant: %+v", path, actual, expected)
		}
		delete(actualFiles, path)
	}

	// Check for unexpected files
	for path := range actualFiles {
		t.Errorf("Unexpected file found: %s", path)
	}
}

type rewriteTransport struct {
	Transport http.RoundTripper
	TestURL   string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, _ := url.ParseRequestURI(t.TestURL)
	req.URL.Scheme = u.Scheme
	req.URL.Host = u.Host
	return t.Transport.RoundTrip(req)
}

func (s *mockServer) Client(t *testing.T) *github.Client {
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	hc := &http.Client{
		Transport: &rewriteTransport{
			Transport: http.DefaultTransport,
			TestURL:   ts.URL,
		},
	}
	return github.NewClient(hc, "test-token", t)
}
