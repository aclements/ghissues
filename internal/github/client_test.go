// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("Expected auth header, got: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Accept") != "application/vnd.github.v3+json" {
			t.Errorf("Expected accept header, got: %q", r.Header.Get("Accept"))
		}

		if r.URL.Path == "/list" {
			w.Header().Set("Link", `<https://api.github.com/res?page=2>; rel="next"`)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`[{"id": 1}, {"id": 2}]`))
		} else {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id": 3}`))
		}
	}))
	defer ts.Close()

	client := NewClient(nil, "test-token", t)

	t.Run("DoRequestList", func(t *testing.T) {
		items, resp, err := client.DoRequestList(ts.URL+"/list", nil)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if resp.NextURL != "https://api.github.com/res?page=2" {
			t.Errorf("Expected next page URL, got: %q", resp.NextURL)
		}
		if len(items) != 2 {
			t.Errorf("Expected 2 items, got %d", len(items))
		}
	})

	t.Run("DoRequestSingle", func(t *testing.T) {
		item, _, err := client.DoRequestSingle(ts.URL+"/single", nil)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		var parsed struct{ ID int }
		if err := json.Unmarshal(item, &parsed); err != nil {
			t.Fatalf("Failed to parse returned item: %v", err)
		}
		if parsed.ID != 3 {
			t.Errorf("Expected id=3, got: %v", parsed.ID)
		}
	})
}

func TestConditionalRequest(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		etag := r.Header.Get("If-None-Match")
		if etag == "test-etag" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", "test-etag")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id": 1}`))
	}))
	defer ts.Close()

	client := NewClient(nil, "", t)

	t.Run("InitialRequest", func(t *testing.T) {
		_, resp, err := client.DoRequestSingle(ts.URL, nil)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if resp.ETag != "test-etag" {
			t.Errorf("Expected ETag test-etag, got %q", resp.ETag)
		}
		if resp.NotModified {
			t.Error("Expected NotModified=false")
		}
	})

	t.Run("ConditionalRequest", func(t *testing.T) {
		item, resp, err := client.DoRequestSingle(ts.URL, &RequestOptions{ETag: "test-etag"})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if !resp.NotModified {
			t.Error("Expected NotModified=true")
		}
		if item != nil {
			t.Errorf("Expected nil item for 304, got %s", string(item))
		}
	})
}

func TestErrors(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantErr    string
		wantAuth   bool
	}{
		{
			name:       "APIError",
			statusCode: http.StatusForbidden,
			body:       `{"message": "Bad credentials", "documentation_url": "https://docs"}`,
			wantErr:    "Bad credentials",
			wantAuth:   true,
		},
		{
			name:       "Not Found",
			statusCode: http.StatusNotFound,
			body:       `{"message": "Not Found"}`,
			wantErr:    "Not Found",
			wantAuth:   false,
		},
		{
			name:       "Malformed JSON",
			statusCode: http.StatusOK,
			body:       `{"id": 1,`,
			wantErr:    "unmarshaling",
			wantAuth:   false,
		},
		{
			name:       "Empty body",
			statusCode: http.StatusOK,
			body:       ``,
			wantErr:    "",
			wantAuth:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				w.Write([]byte(tt.body))
			}))
			defer ts.Close()

			client := NewClient(nil, "", t)

			// Test List variant
			t.Run("DoRequestList", func(t *testing.T) {
				items, _, err := client.DoRequestList(ts.URL, nil)
				verifyError(t, err, tt.wantErr, tt.wantAuth)
				if tt.wantErr == "" && len(items) != 0 {
					t.Errorf("Expected empty list, got %d items", len(items))
				}
			})

			// Test Single variant
			t.Run("DoRequestSingle", func(t *testing.T) {
				item, _, err := client.DoRequestSingle(ts.URL, nil)
				verifyError(t, err, tt.wantErr, tt.wantAuth)
				if tt.wantErr == "" && item != nil {
					t.Errorf("Expected nil item, got %s", string(item))
				}
			})
		})
	}
}

func verifyError(t *testing.T, err error, wantErr string, wantAuth bool) {
	t.Helper()
	if wantErr == "" {
		if err != nil {
			t.Fatalf("Unexpected error, got: %v", err)
		}
		return
	}

	if err == nil {
		t.Fatalf("Expected error containing %q, got nil", wantErr)
	}

	if !strings.Contains(err.Error(), wantErr) {
		t.Errorf("Expected error to contain %q, got: %v", wantErr, err)
	}

	apiErr, isApiErr := err.(*APIError)
	if wantAuth {
		if !isApiErr {
			t.Fatalf("Expected APIError, got %T", err)
		}
		if !apiErr.IsAuthError() {
			t.Error("Expected IsAuthError to be true")
		}
	} else if isApiErr && apiErr.IsAuthError() {
		t.Error("Expected IsAuthError to be false")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestRateLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requests := 0
		hc := &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				if requests == 1 {
					// Simulate a secondary rate
					// limit that asks the client
					// to wait 10 seconds.
					h := make(http.Header)
					h.Set("Retry-After", "10")
					return &http.Response{
						StatusCode: http.StatusTooManyRequests,
						Header:     h,
						Body:       io.NopCloser(bytes.NewReader(nil)),
					}, nil
				}
				if requests == 2 {
					// Simulate a primary rate
					// limit that asks the client
					// to wait 60 seconds.
					h := make(http.Header)
					h.Set("X-RateLimit-Remaining", "0")
					h.Set("X-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Unix()+60))
					return &http.Response{
						StatusCode: http.StatusForbidden,
						Header:     h,
						Body:       io.NopCloser(bytes.NewReader(nil)),
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(bytes.NewReader([]byte(`[]`))),
				}, nil
			}),
		}
		client := NewClient(hc, "", t)

		start := time.Now()
		_, _, err := client.DoRequestList("https://api.github.com/foo", nil)
		duration := time.Since(start)

		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if requests != 3 {
			t.Errorf("Expected 3 requests, got %d", requests)
		}

		// The client should have slept for 10s (secondary) + ~62s (primary with buffer)
		if duration < 70*time.Second || duration > 80*time.Second {
			t.Errorf("Expected simulated duration ~72s, got %v", duration)
		}
	})
}

func TestRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requests := 0
		hc := &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				if requests == 1 {
					return &http.Response{
						StatusCode: http.StatusBadGateway, // 502
						Body:       io.NopCloser(bytes.NewReader(nil)),
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader([]byte(`[]`))),
				}, nil
			}),
		}
		client := NewClient(hc, "", t)

		start := time.Now()
		items, _, err := client.DoRequestList("https://api.github.com/foo", nil)
		duration := time.Since(start)

		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if requests != 2 {
			t.Errorf("Expected 2 requests, got %d", requests)
		}
		if len(items) != 0 {
			t.Errorf("Expected 0 items, got %d", len(items))
		}

		// Should have slept for 2 seconds (1 << 1)
		if duration < 2*time.Second || duration > 3*time.Second {
			t.Errorf("Expected duration ~2s, got %v", duration)
		}
	})
}

func TestExtractNextPageURL(t *testing.T) {
	tests := []struct {
		name       string
		linkHeader string
		expected   string
	}{
		{
			name:       "empty link header",
			linkHeader: "",
			expected:   "",
		},
		{
			name:       "valid next link",
			linkHeader: `<https://api.github.com/repositories/123/issues?page=2>; rel="next", <https://api.github.com/repositories/123/issues?page=10>; rel="last"`,
			expected:   "https://api.github.com/repositories/123/issues?page=2",
		},
		{
			name:       "next link at the end",
			linkHeader: `<https://api.github.com/repositories/123/issues?page=1>; rel="prev", <https://api.github.com/repositories/123/issues?page=3>; rel="next"`,
			expected:   "https://api.github.com/repositories/123/issues?page=3",
		},
		{
			name:       "no next link",
			linkHeader: `<https://api.github.com/repositories/123/issues?page=1>; rel="prev", <https://api.github.com/repositories/123/issues?page=1>; rel="first"`,
			expected:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractNextPageURL(tt.linkHeader)
			if result != tt.expected {
				t.Errorf("extractNextPageURL() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestAPIError(t *testing.T) {
	err := &APIError{
		URL:        "https://api.github.com/foo",
		StatusCode: 404,
		Message:    "Not Found",
	}
	expected := "GitHub API error at https://api.github.com/foo (status 404): Not Found"
	if err.Error() != expected {
		t.Errorf("Expected %q, got %q", expected, err.Error())
	}
}
