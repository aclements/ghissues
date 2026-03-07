// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package github

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// APIError represents an error returned by the GitHub API.
type APIError struct {
	URL        string
	StatusCode int
	Message    string
}

// githubErrorResponse models the standard GitHub API error JSON.
type githubErrorResponse struct {
	Message          string `json:"message"`
	DocumentationURL string `json:"documentation_url,omitempty"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("GitHub API error at %s (status %d): %s", e.URL, e.StatusCode, e.Message)
}

// IsAuthError returns true if the error indicates an authentication or authorization failure.
func (e *APIError) IsAuthError() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// Logger allows a user of the package to provide a status printer.
type Logger interface {
	Logf(format string, args ...any)
}

type noopLogger struct{}

func (noopLogger) Logf(format string, args ...any) {}

// Client is a custom HTTP client for the GitHub REST API.
type Client struct {
	token      string
	httpClient *http.Client
	logger     Logger
}

// NewClient creates a new GitHub API client. If httpClient is nil,
// it uses a default http.Client with a 30 second timeout. If logger is nil,
// a no-op logger is used.
func NewClient(httpClient *http.Client, token string, logger Logger) *Client {
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 30 * time.Second,
		}
	}
	if logger == nil {
		logger = noopLogger{}
	}
	return &Client{
		token:      token,
		httpClient: httpClient,
		logger:     logger,
	}
}

// RequestOptions allows passing additional parameters to API requests.
type RequestOptions struct {
	ETag string
}

// Response contains metadata about the API response.
type Response struct {
	NextURL     string
	ETag        string
	NotModified bool
}

// DoRequestList makes a GET request to the given GitHub API URL and expects a JSON array.
// It handles authentication, rate limit sleeps, and returns the raw JSON
// of the items and the response metadata.
func (c *Client) DoRequestList(urlStr string, opts *RequestOptions) ([]json.RawMessage, *Response, error) {
	body, respMeta, err := c.doRequestBytes(urlStr, opts)
	if err != nil {
		return nil, nil, err
	}

	var items []json.RawMessage
	if len(body) == 0 {
		return nil, respMeta, nil
	}

	if err := json.Unmarshal(body, &items); err != nil {
		return nil, nil, fmt.Errorf("unmarshaling json array from %s: %w", urlStr, err)
	}

	return items, respMeta, nil
}

// DoRequestSingle makes a GET request to the given GitHub API URL and expects a single JSON object.
// It handles authentication, rate limit sleeps, and returns the raw JSON and response metadata.
func (c *Client) DoRequestSingle(urlStr string, opts *RequestOptions) (json.RawMessage, *Response, error) {
	body, respMeta, err := c.doRequestBytes(urlStr, opts)
	if err != nil {
		return nil, nil, err
	}

	if len(body) == 0 {
		return nil, respMeta, nil
	}

	var item json.RawMessage
	if err := json.Unmarshal(body, &item); err != nil {
		return nil, nil, fmt.Errorf("unmarshaling single json object from %s: %w", urlStr, err)
	}

	return item, respMeta, nil
}

func (c *Client) doRequestBytes(urlStr string, opts *RequestOptions) ([]byte, *Response, error) {
	var resp *http.Response

	retryCount := 0
	for resp == nil {
		var err error
		resp, retryCount, err = c.oneRequest(urlStr, opts, retryCount)
		if err != nil {
			return nil, nil, err
		}
	}

	respMeta := &Response{
		ETag: resp.Header.Get("ETag"),
	}

	if resp.StatusCode == http.StatusNotModified {
		resp.Body.Close()
		respMeta.NotModified = true
		return nil, respMeta, nil
	}

	// Handle other HTTP failures
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		apiErr := &APIError{
			URL:        urlStr,
			StatusCode: resp.StatusCode,
			Message:    string(bodyBytes), // Fallback to raw string
		}

		// Try to parse the structured error JSON
		var githubErr githubErrorResponse
		if err := json.Unmarshal(bodyBytes, &githubErr); err == nil && githubErr.Message != "" {
			msg := githubErr.Message
			if githubErr.DocumentationURL != "" {
				msg += fmt.Sprintf(" (see: %s)", githubErr.DocumentationURL)
			}
			apiErr.Message = msg
		}

		return nil, nil, apiErr
	}

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("reading response body from %s: %w", urlStr, err)
	}

	respMeta.NextURL = extractNextPageURL(resp.Header.Get("Link"))

	return body, respMeta, nil
}

// oneRequest performs a single attempt at an HTTP request.
//
// If the request succeeds (including 304 Not Modified) or has a permanent
// HTTP failure, this returns (resp, 0, nil). This includes 200 OK, 4xx errors
// (like 401 or 404), and 5xx errors that have exhausted their max retries.
// The caller is responsible for closing the response body.
//
// If the request encounters a retryable transient error or reaches a
// rate limit, this returns (nil, updatedRetryCount, nil). The method
// will have already performed the necessary sleep. The caller should
// call oneRequest again with the updated retry count. For rate
// limits, the retry count is not incremented.
//
// Finally, (nil, 0, err) indicates a failure to even initiate the
// request (e.g., malformed URL) or a network error after retries are
// exhausted.
func (c *Client) oneRequest(urlStr string, opts *RequestOptions, retryCount int) (*http.Response, int, error) {
	const maxRetries = 5

	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("creating request for %s: %w", urlStr, err)
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if opts != nil && opts.ETag != "" {
		req.Header.Set("If-None-Match", opts.ETag)
	}

	resp, err := c.httpClient.Do(req)

	var transientErr error
	if err != nil {
		// A non-nil error from Do() indicates a failure to speak HTTP
		// entirely. In a long-running sync, this is almost always a
		// transient network issue (DNS failure, connection reset, timeout).
		// While technically some url.Errors could be permanent, retrying
		// all connection failures is a pragmatic choice for this tool.
		transientErr = err
	} else if resp.StatusCode >= 500 && resp.StatusCode <= 599 {
		// Server-side errors (5xx) are by definition transient.
		transientErr = fmt.Errorf("%s", resp.Status)
	} else if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		// Handle Rate Limits (both primary and secondary)
		sleepDuration, isRateLimit, err := c.parseRateLimit(resp)
		if err != nil {
			return nil, 0, fmt.Errorf("handling rate limit for %s: %w", urlStr, err)
		}

		if isRateLimit {
			c.logger.Logf("Rate limit hit. Sleeping for %v\n", sleepDuration)
			resp.Body.Close()
			time.Sleep(sleepDuration)
			return nil, retryCount, nil // Retry, don't increment retryCount
		}
	}

	if transientErr != nil {
		if retryCount >= maxRetries {
			if err != nil {
				if resp != nil && resp.Body != nil {
					resp.Body.Close()
				}
				return nil, 0, fmt.Errorf("doing request for %s after %d retries: %w", urlStr, maxRetries, err)
			}
			// If err is nil, it's a 5xx error. We do not continue the loop,
			// allowing it to fall through to the standard non-200 error handler
			// below which will read, parse, and include the response body.
			return resp, 0, nil
		} else {
			if resp != nil && resp.Body != nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			retryCount++
			sleepDuration := time.Duration(1<<retryCount) * time.Second
			c.logger.Logf("Transient error fetching %s: %s. Retrying in %v...\n", urlStr, transientErr, sleepDuration)
			time.Sleep(sleepDuration)
			return nil, retryCount, nil // Retry
		}
	}

	// If we reach here, it's not a rate limit sleep, and not a retriable transient error.
	// It could be a 200 OK, a 304 Not Modified, a 404, an auth error, or a 5xx that hit the retry limit.
	return resp, 0, nil
}

// parseRateLimit inspects the response headers to determine if the response
// is a rate limit error. It returns the duration to sleep, a boolean indicating
// if it is a rate limit, and an error.
func (c *Client) parseRateLimit(resp *http.Response) (time.Duration, bool, error) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return 0, false, nil
	}

	remaining := resp.Header.Get("X-RateLimit-Remaining")
	retryAfter := resp.Header.Get("Retry-After")

	if retryAfter != "" {
		// Secondary rate limit handling
		seconds, parseErr := strconv.Atoi(retryAfter)
		if parseErr != nil {
			return 0, true, fmt.Errorf("malformed Retry-After header (%s): %w", retryAfter, parseErr)
		}
		return time.Duration(seconds) * time.Second, true, nil
	} else if remaining == "0" {
		// Primary rate limit handling
		resetStr := resp.Header.Get("X-RateLimit-Reset")
		resetUnix, parseErr := strconv.ParseInt(resetStr, 10, 64)
		if parseErr != nil {
			return 0, true, fmt.Errorf("malformed X-RateLimit-Reset header (%s): %w", resetStr, parseErr)
		}
		resetTime := time.Unix(resetUnix, 0)
		sleepDuration := time.Until(resetTime)

		// Add a small buffer to ensure we wake up after the reset
		if sleepDuration < 0 {
			sleepDuration = 0
		}
		sleepDuration += 2 * time.Second
		return sleepDuration, true, nil
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		// Fallback if headers are missing but we explicitly got a rate limit status
		return 1 * time.Minute, true, nil
	}

	// A 403 without rate limit headers is a normal auth or permission error
	return 0, false, nil
}

// extractNextPageURL parses the standard GitHub API Link header to find the "next" page URL.
func extractNextPageURL(linkHeader string) string {
	if linkHeader == "" {
		return ""
	}

	links := strings.Split(linkHeader, ",")
	for _, link := range links {
		parts := strings.Split(link, ";")
		if len(parts) < 2 {
			continue
		}

		urlPart := strings.TrimSpace(parts[0])
		relPart := strings.TrimSpace(parts[1])

		if strings.HasPrefix(urlPart, "<") && strings.HasSuffix(urlPart, ">") && relPart == `rel="next"` {
			return urlPart[1 : len(urlPart)-1]
		}
	}
	return ""
}
