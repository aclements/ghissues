# ghissues Data Format

The `ghissues` tool stores raw JSON responses directly from the GitHub
API into a predictable directory structure.

Because the data is raw, it is future-proof: if GitHub adds new fields
to their API, the tool automatically begins capturing them.

## Directory Structure

Data is stored relative to the root directory provided to the tool
(defaults to `_mirror/`). Inside the root, data is separated by the
repository owner and name.

The core data is grouped by **Issue Number**. Inside each issue's
directory, all associated data (the top-level issue itself, user
comments, and metadata events) is stored in a flat list.

```text
_mirror/
└── {owner}/
    └── {repo}/
        ├── sync_state.json
        └── issues/
            ├── 1/
            │   ├── issue.json
            │   ├── 2023-10-25T10:00:00Z-event-1111.json
            │   ├── 2023-10-25T10:05:00Z-comment-2222.json
            │   └── ...
            ├── 2/
            ...
```

## File Types & Naming Convention

By listing an issue's directory sorted alphabetically, you get a
chronological timeline of everything that happened to that issue.

### 1. `issue.json`

The raw JSON object representing the top-level issue (the original
post, title, current labels, current state, and summary reactions).

Corresponds to the payload returned by the
[Get an issue](https://docs.github.com/en/rest/issues/issues?apiVersion=2022-11-28#get-an-issue)
API.

**Edits:** This file is overwritten whenever the issue body is edited
or its metadata (labels, assignees, state) changes. Previous states
can be found by inspecting the Git commit history of this file.

### 2. `YYYY-MM-DDTHH:MM:SSZ-comment-{id}.json`

The raw JSON object representing a user comment on the issue.

Corresponds to the payload returned by the
[Get an issue comment](https://docs.github.com/en/rest/issues/comments?apiVersion=2022-11-28#get-an-issue-comment)
API.

**Edits:** The timestamp in the filename is the `created_at` time of
the comment. If a user later edits the comment, this same file is
overwritten with the new payload. The Git commit history will contain
the diff of the edit.

### 3. `YYYY-MM-DDTHH:MM:SSZ-event-{id}.json`

The raw JSON object representing a discrete metadata change (e.g., a
label was added, the issue was closed, a user was assigned).

Corresponds to the payloads returned by the
[List issue events](https://docs.github.com/en/rest/issues/events?apiVersion=2022-11-28#list-issue-events-for-a-repository)
API.

**Immutability:** Events are historical logs and are immutable. Once
written, these files are never modified.

## `sync_state.json`

This is an internal file used by the tool to track its pagination
progress and timestamps to enable fast incremental updates. It should
generally be ignored during analysis.

## Incremental Analysis & Cache Invalidation

If you are building an analysis tool on top of this mirror that
requires expensive processing, you can use the underlying Git
repository to perform efficient partial cache invalidation.

Because `ghissues` automatically commits its changes, a downstream
client can easily determine exactly what files have changed since the
client's last analysis run.

1.  Your analysis tool records the Git commit hash (`git rev-parse
    HEAD`) after it finishes a successful run.
2.  On the next run, your tool asks Git for the list of changed files:
    ```bash
    git diff --name-only <last_processed_commit_hash> HEAD
    ```
3.  Git will output the exact paths that were added, modified, or
    deleted (e.g., `_mirror/golang/go/issues/1234/issue.json`).
4.  Your tool can parse the issue numbers from these paths and
    invalidate only the caches for those specific issues.
