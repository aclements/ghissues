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

The tool uses four different REST data streams:

- Issues (`/repos/.../issues`): The repo-wide stream of issues. Includes
  the top post of each issue.
- Comments (`/repos/.../issues/comments`): The repo-wide stream of
  comments to all issues.
- Repo-wide issue events (`/repos/.../issues/events`): The repo-wide
  stream of events on all issues. This does not include comments, so
  there's no duplication with the comments stream.
- Per-issue events (`/repos/.../issues/{issue}/events`): The per-issue
  stream of events. This is identical to the repo-wide issue events, but
  scoped to a single issue.

All GitHub streams have a pagination limit of 300 pages (or 30,000
items). We handle this differently for different types of streams.

The four streams fall into three categories:

- *Filtered ascending streams* (issues, comments). These can be filtered
  by time and yield the oldest events first. For these, we start the
  stream at the latest timestamp we've seen and fetch as many pages from
  the stream as we can. If we encounter a page limit, we can simply
  start over at the latest timestamp we've seen.

- *Unfiltered descending streams* (repo-wide issue events). These yield
  events starting with the newest first. For these, we start at the
  beginning of the stream (we have no other choice) and fetch pages
  until we see an object who's timestamp is past the last timestamp of a
  full stream or reach the end of the stream. Only at that point do we
  update the full stream timestamp. This has the effect of breaking the
  stream into "segments", where we have to fetch a complete segment
  before we start back at the beginning with a new segment. The only
  solution to the page limit is to fall back to another mechanism: for
  repo-wide issue events, we fall back to per-issue events.

- *Unfiltered ascending streams* (per-issue events). For these, we
  always have to fetch the whole stream, but make aggressive use of ETag
  filtering. These are only used as a fallback mechanism. If we
  encounter a page limit, there's no recourse, but this is unlikely for
  per-issue events.

The per-issue events streams are only used if we have to employ the
"backfill" strategy: if the repo-wide events stream exhausts without
catching up to the previously synced state, that means there may be a
gap in the event history, so the tool switches to a per-issue backfill
process. This ensures 100% event coverage, without having to employ an
expensive per-issue process on every sync.

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
    allowing us to fetch all new comments or events event across a
    repository with a huge number of issues in just a few API calls.

## 3. Sync Process

The full sync state is captured by the `state` type, which is saved to
disk after every page fetch in order to safely handle interruptions. The
sync loop maintains no state outside of the `state` type.

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
Comments, Events, and Backfill) stores its full state in this object and
each iteration of the sync loop simply updates this single source of
state. This is akin to "stack ripping", but forces us to be honest about
what state is saved to disk, and thus what state we can resume from.
This approach means there's virtually no difference between resuming an
interrupted sync and each iteration of the regular sync loop, which
significantly reduces the change of bugs.

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
