# Instructions for AI Agents

## Analyzing the issue mirror

The `_mirror/{owner}/{repo}` directory contains a complete mirror of the GitHub
issue tracker for GitHub repository `{owner}/{repo}`. When building a tool to
analyze the issue mirror, see [FORMAT.md](FORMAT.md) for the file structure of
the mirror.

## Working on the codebase

When working on this codebase, please adhere to the following guidelines:

### Code Changes

This project strictly follows standard Go formatting. After making any
modifications to the Go source code, you **must** run:

```bash
go fmt ./...
```

Please ensure that the code compiles (`go build ./...`) and all tests pass (`go
test ./...`) before concluding your work.

### Code Authoring

Exported functions and methods **must** have documentation comments explaining
their purpose, arguments, and results from the perspective of a caller.
Unexported functions should follow this same guidance unless they are very
simple. If you change the implementation of a function, be sure to update its
documentation comment if appropriate. If you make a change that requires a
documentation comment update, check the callers of the function to see if they
also need documentation updates.

### Project Documentation

The architecture and data structure of this project are strictly documented. If
you make any changes that affect how the tool works or how the data is stored,
you **must** update the relevant documentation:

*   **[FORMAT.md](FORMAT.md):** Describes the exact layout, file naming
    conventions, and data schemas of the downloaded mirror data. If you change
    how data is written to disk (e.g., adding a new file type, changing the
    directory structure), you must update this file.
*   **[DESIGN.md](DESIGN.md):** Details the overall architecture, sync process,
    and justifications for design decisions. If you alter the internal flow, API
    usage, or the incremental sync logic, you must update this document to
    reflect the new reality.
