# GitHub Repository Mirror Tool - Design & Implementation

This document outlines the architectural decisions and justifications
behind `ghissues`.

For details on the actual data layout and how to interpret the
mirrored files, see [FORMAT.md](FORMAT.md).

## 1. Architecture & Storage Strategy

**Approach:** Raw responses stored as flat data in a git repository.

The tool uses a "Git scraping" approach. It stores the raw JSON
responses from the GitHub API directly as files on the filesystem,
which is itself stored in a Git repository.

**Rationale:**
*   **Future-proofing (raw JSON):** Storing the raw JSON ensures that
    if GitHub adds new fields to their API in the future, we
    automatically capture them without needing to update database
    schemas or Go structs.
*   **Ad-hoc tooling:** Storing data as flat files makes it easily
    accessible to standard UNIX tools (`jq`, `grep`, `rg`, `find`) and
    concurrent scripts for quick analysis without needing a SQL
    engine.
*   **Free edit history (Git):** By relying on Git commits after each
    sync, we get a complete history of edits to issue descriptions and
    comments for free, without designing complex temporal database
    schemas. This also enables tools to efficiently determine what's
    changed in the mirror. See [FORMAT.md](FORMAT.md) for how file
    naming enables this.

## 2. GitHub API Strategy

The tool uses the GitHub REST API, specifically the repository-wide
endpoints.

**Data Streams:**
1.  **Issues:** `GET /repos/:owner/:repo/issues?state=all&since={last_sync}`
2.  **Comments:** `GET /repos/:owner/:repo/issues/comments?since={last_sync}`
3.  **Events:** `GET /repos/:owner/:repo/issues/events`

**Rationale:**
*   **Why REST over GraphQL?** GraphQL suffers from the "nested
    pagination problem." Fetching 100 issues and then paginating
    through potentially hundreds of comments for *each* of those 100
    issues requires complex cursor management and quickly exhausts API
    complexity limits. The REST API allows us to treat Issues and
    Comments as two simple, flat, independent streams.
*   **Why not the Timeline API?** The Timeline API
    (`/repos/.../issues/{number}/timeline`) combines events and
    comments perfectly, but it is a *per-issue* endpoint. To sync
    updates, we would have to make an API call for every single issue
    that changed. The repository-wide endpoints act as a "firehose",
    allowing us to fetch all new comments or events across the entire
    70,000+ issue repository in just a few API calls.
*   **Duplication:** The repository-wide Issue Events API specifically
    excludes comments (which are technically events). Therefore, our
    Comments stream and Events stream are mutually exclusive; there is
    no data duplication.

**Pagination & Limits:**
GitHub's REST API enforces a hard pagination limit of 300 pages (approx
30,000 items) for generic list endpoints. For a repository like
`golang/go` with over 400,000 comments, standard pagination would stop
early.

To solve this, whenever `ghissues` reaches the end of an Issues or
Comments stream (indicated by a missing "next" Link header), it
automatically attempts to "stitch" the firehose. It synthesizes a brand
new API request using the `since` parameter set to the timestamp of the
newest item seen so far. If this fresh URL still results in zero items,
then the stream is done. This allows the tool to seamlessly traverse an
infinite number of items over time.

The Events stream does not support the `since` parameter, so it cannot
bypass the 30,000 item limit directly. To handle this, `ghissues`
employs a "Backfill" strategy: if the repository-wide events firehose
exhausts without catching up to the previously synced state, the tool
identifies a history gap and automatically transitions into an
interleaved per-issue backfill process. This ensures 100% event fidelity
by fetching events directly from each individual issue's endpoint
(`/repos/.../issues/{number}/events`).

## 3. Sync Process

The tool syncs several sources of state from GitHub. The full sync state is
captured by the `state` type, which is saved to disk after every page fetch in
order to safely handle interruptions. The sync loop maintains no state outside
of the `state` type.

There are three categories of data streams:

- Ascending streams (oldest first) with "since" filtering (issues, comments).
  For these, we start the stream at the latest timestamp we've seen and fetch as
  many pages from the stream as we can.

- Ascending streams (oldest first) with no time filtering (per-issue events).
  For these, we always have to fetch the whole stream. ETag filtering is
  effective for this type of stream, but generally we try to avoid using these
  streams.

- Descending streams (newest first) with no filtering (repo-wide events). For
  these, we start at the beginning of the stream (we have no other choice) and
  fetch pages until we see an object who's timestamp is past the last timestamp
  of a full stream or reach the end of the stream. Only at that point do we
  update the full stream timestamp. This has the effect of breaking the stream
  into "segments", where we have to fetch a complete segment before we start
  back at the beginning.

The sync algorithm proceeds as follows:

1.  **Read State:** Load `state` from `sync_state.json`.
2.  **Sequential Fetching:** For each data stream (Issues, Comments,
    Events, Backfill), loop continuously until the stream is exhausted
    (no `NextURL` and no initialization logic remaining):
    *   Fetch 1 page of the current stream. For descending streams like
        Events, fetching stops early if an item is older than the stream's
        `StopTime`.
    *   The Backfill stream is a "2-D" stream indexed by (issue number, issue
        events page). When it finishes the events stream for an issue, it moves
        on to the next issue.
    *   **Backfill Detection:** By default, the Backfill stream is empty. If the
        Events stream exhausts without hitting its `StopTime`, this indicates a
        gap and we initiate the Backfill stream.
    *   **Continuous State Saving:** The stream state is updated in memory and
        saved to `sync_state.json` after *every* page.
3.  **Finalize & Commit:** Once all streams are exhausted, the tool
    runs `git add .` and `git commit`.

### Unified Resumption

The synchronization state reflected in `sync_state.json` is the single
source of truth for the entire process. Each data stream (Issues,
Comments, Events, and Backfill) stores its full state in this object and each
iteration of the sync loop simply updates this single source of state.
This means there's virtually no difference between resuming an
interrupted sync and each iteration of the regular sync loop. This
unified approach reduces the risk of bugs during sync resumption.

## 4. Implementation Details

1.  **Custom HTTP Client (`internal/github`):** A client that handles
    GitHub authentication via access tokens, rate limiting (inspecting
    `X-RateLimit-*` headers with exponential backoff), and `Link`
    header pagination. Crucially, this client returns raw `[]byte`
    JSON so we can capture the complete response in the mirror.
2.  **Sync Logic (`internal/ghsync`):** Implements the sequential
    loops for the three data streams and handles atomic state file
    writing.
3.  **Storage Layer:** Handles parsing the raw JSON *just enough* to
    extract IDs and timestamps, formats the JSON with indentation (for
    clean Git diffs), and writes to the directory structure.
