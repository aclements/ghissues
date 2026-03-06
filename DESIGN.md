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
infinite number of items over time. The Events stream does not support
the `since` parameter, so it cannot bypass the 30,000 item limit.

## 3. Sync Process

The tool performs resumable, interleaved syncs to remain fast, respect
rate limits, and safely handle interruptions during massive initial
downloads.

1.  **Read State:** Load `sync_state.json`. If it's a new sync pass,
    initialize the `next` URLs for Issues, Comments, and Events.
    Issues and Comments use `since={last_sync}` and are explicitly
    sorted chronologically (`sort=updated&direction=asc`).
2.  **Interleaved Fetching:** Loop continuously while any stream has a
    `next` URL:
    *   Fetch 1 page of Issues (saving `issue.json`).
    *   Fetch 1 page of Comments (saving `{created_at}-comment-{id}.json`).
    *   Fetch 1 page of Events (saving
        `{created_at}-event-{id}.json`). For incremental syncs,
        fetching events stops early by checking if the event's
        `created_at` timestamp is older than `last_event_sync`.
    *   **Stitching:** If an Issues or Comments stream hits GitHub's
        internal pagination wall, synthesize a new `since` URL to
        continue seamlessly (see "Pagination & Limits" above).
    *   Save the current `next` URLs to `sync_state.json` after
        *every* page. If the process is interrupted, the next run
        resumes exactly where it left off.
3.  **Update State & Commit:** Once all streams are exhausted, the
    tool updates `last_issue_sync`, `last_comment_sync`, and
    `last_event_sync` with the exact timestamps observed during the
    pass and runs `git add .` and `git commit`.

## 4. Implementation Details

1.  **Custom HTTP Client (`internal/github`):** A client that handles
    GitHub authentication via access tokens, rate limiting (inspecting
    `X-RateLimit-*` headers with exponential backoff), and `Link`
    header pagination. Crucially, this client returns raw `[]byte`
    JSON so we can capture the complete response in the mirror.
2.  **Sync Logic (`internal/ghsync`):** Implements the interleaved
    loops for the three data streams and handles atomic state file
    writing.
3.  **Storage Layer:** Handles parsing the raw JSON *just enough* to
    extract IDs and timestamps, formats the JSON with indentation (for
    clean Git diffs), and writes to the directory structure.
