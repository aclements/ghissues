# Plan for Event Backfill Mode with ETags

## Objective
Fix the issue where historical events are missing due to the GitHub REST API's 30,000 item limit on the repository-wide `/issues/events` endpoint. We will implement an auto-triggered "Event Backfill Mode" that uses per-issue event endpoints, optimized with ETags to minimize API quota and bandwidth usage.

After each step, STOP to allow human review. DO NOT proceed to the next step until told to do so.

## 1. Refactor Stream State Management [COMPLETED]
*   Modify `internal/ghsync/pagestream.go` to separate `pageStream` into a configuration struct (`pageStream`) and a state struct (`streamState`).
*   The `streamState` struct should contain the persistent fields currently in `pageStream` (`nextURL`, `newest`, `stopTime`).
*   Update `internal/ghsync/state.go` to include `streamState` fields directly or as nested structs for Issues, Comments, and Events, rather than tracking these fields loosely. This makes the `ghsync.state` object the single source of truth.
*   Update `fetchNext` to accept a pointer to the relevant `streamState` and update it directly, ensuring consistency between loop iterations and resumptions.
*   This refactoring will change the format of `sync_state.json`.

## 2. Implement Basic Sync Tests
*   Implement a simple test HTTP server that responds to the requests used by the syncer based on a minimal in-memory corpus of issues, comments, and events. You will be implementing tests using this test server, the `httptest` package, and overriding the `github.Client.Transport` to redirect all requests to the test server. This test server should include a method that checks that an on-disk mirror directory matches its in-memory state.
*   Write a "smoke test" that tests the basic functionality of the syncer. Run of the syncer and make sure the output is as expected. Then, to make sure updates work, run the syncer again with no changes, check the output, then add to the test server's issues, comments, and events, run the syncer once more and check for the expected output.
*   Run this test and make sure it passes.

## 3. Implement Sync Resume Tests
*   Using the test server you implemented in step 2, we're going to test sync resume logic by injecting HTTP errors that cause the sync loop to abort. Add a `resumeTest` flag to the test server that causes it to track all of the request URLs it's seen. If it gets a new request URL, it responds to that as usual but sets a `failNext` flag. If the `failNext` flag is set, it responds with a non-transient HTTP error.
*   Using the same test server corpus as the smoke test, enable `resumeTest` mode and run the syncer in a loop. After each return of the syncer, clear the `failNext` flag. Once the syncer completes without failing, check that the on-disk mirror is correct and complete.

## 4. Backfill State & Gap Detection
*   Modify `internal/ghsync/state.go` to add a `Backfill` state struct to the global `state`. 
*   The `Backfill` struct should track the ongoing backfill process: `CurrentIssue int`, and a `Stream streamState` to handle the pagination of the per-issue event list.
*   Update the event synchronization logic: if the event stream naturally exhausts (hits the API limit, meaning no next URL) without having stopped due to reaching an event older than the stop time, we have a gap. When this gap is detected, initialize the `Backfill` state to begin backfilling.

## 5. Event Backfill Stream Implementation
*   Create a mechanism (e.g., a `backfillStream` struct or a new method on `syncer`) that integrates directly into the main interleaved `sync` loop in `sync.go`.
*   While `state.Backfill.CurrentIssue` indicates an active backfill process:
    *   Use the `state.Backfill.Stream` (which is a `streamState`) to manage pagination. If `NextURL` is empty, construct the per-issue events URL.
    *   Fetch the page using `DoRequestList` (initially without ETags, see Step 6).
    *   When all pages for the issue are fetched (i.e. `Stream.NextURL` becomes empty after processing), update `state.Backfill.CurrentIssue` to the next issue and reset `state.Backfill.Stream`.
    *   If `200 OK`, process and save the events using the existing `{created_at}-event-{id}.json` format. Update `state.Backfill.Stream.NextURL`.

## 6. Integration
*   Add the backfill processing step to the `for` loop in `sync.go` alongside issues, comments, and events. This ensures that backfill progress is continually saved to `sync_state.json` and perfectly resumable if interrupted.

## 7. GitHub Client Modifications
*   Update `internal/github/client.go` to support conditional requests with ETags using the existing `DoRequest` and `DoRequestList` methods.
*   Create a `RequestOptions` struct with an `ETag string` field to pass conditional headers. Add this as an argument to the existing `DoRequest` and `DoRequestList` methods.
*   Create a `Response` struct to encapsulate metadata returned by requests: `NextURL string`, `ETag string`, and `NotModified bool`.
*   Update both `DoRequest` and `DoRequestList` to return `(data, *Response, error)` instead of just the next URL, simplifying the return signature while providing the necessary ETag and 304 status information.

## 8. Per-Issue Sync State (Optimization)
*   Define a new struct `IssueSyncState` with an `EventsETag string` field.
*   Store this state in `<baseDir>/issues/<issue_number>/sync_state.json`.
*   Create helper functions to load and save this per-issue state, similar to `loadState` and `saveState` for the repo-wide state.
*   Update the `Event Backfill Stream Implementation` (Step 4) to use `EventsETag` for conditional requests to save API quota on issues whose events haven't changed.