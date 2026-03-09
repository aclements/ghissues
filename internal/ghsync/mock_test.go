// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ghsync

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

	// fetches counts successful fetches. The caller is allowed to reset this.
	fetches int

	// maxFetches is a limit on fetches to prevent infinite loops.
	maxFetches int

	// forceBackfill causes the repo-wide issue events endpoint to return an
	// empty list, forcing a backfill from the per-issue events endpoints.
	forceBackfill bool

	// servedIssueEvents is set if the server served a request for issue events.
	servedIssueEvents bool

	// testResume enables "resumption testing" mode, where the first time the
	// server gets a request for a new URL, it will set failAll to enter failure
	// mode, which causes it to respond to this and all further requests with
	// "Bad Request". The caller can reset failAll and retry, which will allow a
	// sequent request for the previously failed URL to succeed.
	testResume bool
	failAll    bool
	seenURLs   map[string]bool
}

func newMockServer(t *testing.T) *mockServer {
	return &mockServer{
		t:          t,
		seenURLs:   make(map[string]bool),
		nextID:     1,
		maxFetches: 200,
	}
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

	path := r.URL.Path
	pageStr := r.URL.Query().Get("page")
	page := 1
	if pageStr != "" {
		fmt.Sscanf(pageStr, "%d", &page)
	}
	if page < 1 {
		page = 1
	}

	sinceStr := r.URL.Query().Get("since")
	var since time.Time
	if sinceStr != "" {
		since, _ = time.Parse(time.RFC3339, sinceStr)
	}

	direction := r.URL.Query().Get("direction")
	if direction != "asc" && direction != "desc" {
		direction = "desc"
	}

	var resp any
	var hasMore bool

	if strings.Contains(path, "/issues/comments") {
		resp, hasMore = filterAndPage(s.Comments, since, page, direction, func(c mockComment) time.Time { return c.UpdatedAt })
	} else if strings.Contains(path, "/issues/events") {
		if direction != "desc" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		events := s.Events
		if s.forceBackfill {
			events = nil
		}
		resp, hasMore = filterAndPage(events, since, page, direction, func(e mockEvent) time.Time { return e.CreatedAt })
	} else if strings.HasSuffix(path, "/events") && strings.Contains(path, "/issues/") {
		s.servedIssueEvents = true
		parts := strings.Split(path, "/")
		issueNumStr := parts[len(parts)-2]
		var issueNum int
		fmt.Sscanf(issueNumStr, "%d", &issueNum)

		var issueEvents []mockEvent
		for _, e := range s.Events {
			if e.Issue != nil && e.Issue.Number == issueNum {
				issueEvents = append(issueEvents, e)
			}
		}
		resp, hasMore = filterAndPage(issueEvents, since, page, "asc", func(e mockEvent) time.Time { return e.CreatedAt })
	} else if strings.Contains(path, "/issues") {
		resp, hasMore = filterAndPage(s.Issues, since, page, direction, func(i mockIssue) time.Time { return i.UpdatedAt })
	} else {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if s.fetches >= s.maxFetches {
		s.t.Errorf("max fetch limit (%d) reached; infinite loop?", s.maxFetches)
		w.WriteHeader(http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Type", "application/json")
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
	json.NewEncoder(w).Encode(resp)
	s.fetches++
}

func filterAndPage[T any](data []T, since time.Time, page int, direction string, timestamp func(T) time.Time) ([]T, bool) {
	const pageLen = 10

	// Restore sort of backing data, if necessary.
	if timestamp != nil {
		dir := 1
		if direction == "desc" {
			dir = -1
		}
		cmp := func(a, b T) int { return dir * timestamp(a).Compare(timestamp(b)) }
		if !slices.IsSortedFunc(data, cmp) {
			slices.SortFunc(data, cmp)
		}
	}

	// Filter by "since".
	var filtered []T
	if since.IsZero() || timestamp == nil {
		filtered = data
	} else {
		i, _ := slices.BinarySearchFunc(data, since, func(d T, key time.Time) int {
			return timestamp(d).Compare(key)
		})
		filtered = data[i:]
	}

	// Get page.
	start := min((page-1)*pageLen, len(filtered))
	end := min(start+pageLen, len(filtered))
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

		if relPath == "sync_state.json" {
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
