// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ghsync

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aclements/ghmirror/internal/github"
)

// makeIssue Helper to generate fake items for tests
func makeIssue(id int64, number int, created, updated string) string {
	return fmt.Sprintf(`{"id":%d, "number":%d, "created_at":"%s", "updated_at":"%s"}`, id, number, created, updated)
}

func TestPageStream(t *testing.T) {
	mux := http.NewServeMux()

	baseTime := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)
	t1 := baseTime.Add(-2 * time.Hour)
	t2 := baseTime.Add(-1 * time.Hour)

	mux.HandleFunc("/repos/test/repo/issues", func(w http.ResponseWriter, r *http.Request) {
		since := r.URL.Query().Get("since")
		page := r.URL.Query().Get("page")

		if since == "" {
			if page == "" {
				w.Header().Set("Link", fmt.Sprintf(`<%s/repos/test/repo/issues?page=2>; rel="next"`, "http://"+r.Host))
				w.Write([]byte(fmt.Sprintf(`[%s]`, makeIssue(1, 1, t1.Format(time.RFC3339), t1.Format(time.RFC3339)))))
				return
			}
			if page == "2" {
				// End of standard pagination
				w.Write([]byte(fmt.Sprintf(`[%s]`, makeIssue(2, 2, t2.Format(time.RFC3339), t2.Format(time.RFC3339)))))
				return
			}
		}

		// Expected synthesized URL query
		if since == t2.Format(time.RFC3339) {
			w.Write([]byte("[]"))
			return
		}

		t.Fatalf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := github.NewClient(ts.Client(), "", nil)
	dir := t.TempDir()

	ps := &pageStream{
		client: client,
		pathFunc: func(meta *metadata) (string, error) {
			num, _ := meta.issueNumber()
			return filepath.Join(dir, fmt.Sprintf("%d.json", num)), nil
		},
		initURL: func(since time.Time) string {
			url := ts.URL + "/repos/test/repo/issues"
			if !since.IsZero() {
				url += "?since=" + since.Format(time.RFC3339)
			}
			return url
		},
	}

	// Fetch first page
	if !ps.active() {
		t.Fatal("expected stream to be active")
	}
	if err := ps.fetchNext(); err != nil {
		t.Fatal(err)
	}
	if !ps.madeChange {
		t.Error("expected madeChange=true")
	}
	ps.madeChange = false
	if ps.newest != t1 {
		t.Errorf("expected newest %v, got %v", t1, ps.newest)
	}

	// Fetch second page
	if err := ps.fetchNext(); err != nil {
		t.Fatal(err)
	}
	if !ps.madeChange {
		t.Error("expected madeChange=true")
	}
	ps.madeChange = false
	if ps.newest != t2 {
		t.Errorf("expected newest %v, got %v", t2, ps.newest)
	}

	// Should have synthesized new URL based on newest
	expectedNext := ts.URL + "/repos/test/repo/issues?since=" + t2.Format(time.RFC3339)
	if ps.nextURL != expectedNext {
		t.Errorf("expected nextURL %q, got %q", expectedNext, ps.nextURL)
	}

	// Fetch third page (the restart, returning 0 items)
	if !ps.active() {
		t.Fatal("expected stream to be active for restart")
	}
	if err := ps.fetchNext(); err != nil {
		t.Fatal(err)
	}
	if ps.madeChange {
		t.Error("expected madeChange=false for empty page")
	}
	if ps.active() {
		t.Error("expected stream to be done")
	}

	// Try one more following restart.
	if err := ps.fetchNext(); err != nil {
		t.Fatal(err)
	}
	if ps.madeChange {
		t.Error("expected madeChange=false for empty page")
	}
	if ps.active() {
		t.Error("expected stream to be done")
	}

	// Verify files were written
	if _, err := os.Stat(filepath.Join(dir, "1.json")); err != nil {
		t.Errorf("expected 1.json to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "2.json")); err != nil {
		t.Errorf("expected 2.json to exist: %v", err)
	}
}

func TestPageStream_StopTime(t *testing.T) {
	mux := http.NewServeMux()

	t1 := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC) // Newest
	t2 := t1.Add(-1 * time.Hour)                       // Middle
	t3 := t1.Add(-2 * time.Hour)                       // Oldest, before stopTime

	mux.HandleFunc("/repos/test/repo/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/test/repo/events?page=2>; rel="next"`, "http://"+r.Host))
		w.Write([]byte(fmt.Sprintf(`[%s, %s, %s]`,
			makeIssue(3, 3, t1.Format(time.RFC3339), t1.Format(time.RFC3339)),
			makeIssue(2, 2, t2.Format(time.RFC3339), t2.Format(time.RFC3339)),
			makeIssue(1, 1, t3.Format(time.RFC3339), t3.Format(time.RFC3339)),
		)))
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := github.NewClient(ts.Client(), "", nil)
	dir := t.TempDir()

	stopTime := t2.Add(30 * time.Minute) // Between t2 and t1

	ps := &pageStream{
		client:   client,
		nextURL:  ts.URL + "/repos/test/repo/events",
		stopTime: stopTime,
		pathFunc: func(meta *metadata) (string, error) {
			num, _ := meta.issueNumber()
			return filepath.Join(dir, fmt.Sprintf("%d.json", num)), nil
		},
	}

	if err := ps.fetchNext(); err != nil {
		t.Fatal(err)
	}

	if ps.active() {
		t.Error("expected stream to be done due to stopTime")
	}

	// Should only have written items >= stopTime (so, item 3)
	if _, err := os.Stat(filepath.Join(dir, "3.json")); err != nil {
		t.Errorf("expected 3.json to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "2.json")); !os.IsNotExist(err) {
		t.Errorf("expected 2.json to NOT exist")
	}
	if _, err := os.Stat(filepath.Join(dir, "1.json")); !os.IsNotExist(err) {
		t.Errorf("expected 1.json to NOT exist")
	}
}

func TestPageStream_IdenticalContents(t *testing.T) {
	mux := http.NewServeMux()

	t1 := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)
	itemJSON := makeIssue(1, 1, t1.Format(time.RFC3339), t1.Format(time.RFC3339))

	mux.HandleFunc("/repos/test/repo/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(fmt.Sprintf(`[%s]`, itemJSON)))
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := github.NewClient(ts.Client(), "", nil)
	dir := t.TempDir()

	pathFunc := func(meta *metadata) (string, error) {
		num, _ := meta.issueNumber()
		return filepath.Join(dir, fmt.Sprintf("%d.json", num)), nil
	}

	// Pre-create the file with formatted JSON
	var prettyJSON bytes.Buffer
	json.Indent(&prettyJSON, []byte(itemJSON), "", "  ")
	prettyJSON.WriteByte('\n')

	os.MkdirAll(dir, 0755)
	os.WriteFile(filepath.Join(dir, "1.json"), prettyJSON.Bytes(), 0644)

	ps := &pageStream{
		client:   client,
		nextURL:  ts.URL + "/repos/test/repo/issues",
		pathFunc: pathFunc,
	}

	if err := ps.fetchNext(); err != nil {
		t.Fatal(err)
	}

	if ps.madeChange {
		t.Error("expected madeChange=false because contents were identical")
	}
}
