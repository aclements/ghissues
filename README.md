# ghissues: GitHub issues mirror tool

`ghissues` is a fast, efficient CLI tool designed to create and
maintain a local, fully version-controlled mirror of a GitHub
repository's issue tracker.

It downloads the data as raw JSON files organized into a clean,
chronological directory structure and automatically tracks edits over
time using Git. This makes the data easy to query and analyze using
standard UNIX tools or custom scripts.

## Usage

```bash
go run ./cmd/ghissues [flags] <owner/repo>...
```

See `go run ./cmd/ghissues -h` for additional flags.

### Examples

Mirror a single repository into the default `./_mirror` directory:

```bash
go run ./cmd/ghissues golang/go
```

Mirror multiple repositories into a custom directory:

```bash
go run ./cmd/ghissues -dir /mnt/data/my_mirrors golang/go octocat/Hello-World
```

## Authentication

To respect rate limits and allow for massive initial downloads,
`ghissues` requires a GitHub Fine-Grained Personal Access Token.

The token needs **Read-only** access to the **Issues** and
**Metadata** scopes.

You can provide the token in two ways:
1.  **Environment Variable:** Set `GITHUB_TOKEN=your_token_here`.
2.  **Interactive Prompt:** If you run the tool without the
    environment variable set, it will securely prompt you to paste
    your token. It will then save this token to your local OS
    configuration directory (e.g., `~/.config/ghissues/github.tok`) so
    you never have to enter it again.

## Data Layout & Design

For information on how the data is structured on disk and how to
interpret the JSON files, see [FORMAT.md](FORMAT.md).

For information on the architecture and design decisions behind the
tool, see [DESIGN.md](DESIGN.md).
