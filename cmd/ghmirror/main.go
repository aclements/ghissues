// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/aclements/ghmirror/internal/ghsync"
	"github.com/aclements/ghmirror/internal/github"
)

type cliReporter struct {
	lastMsg string
}

func (r *cliReporter) Logf(format string, args ...any) {
	if r.lastMsg != "" {
		fmt.Println() // Break the progress line
		r.lastMsg = ""
	}
	fmt.Printf(format, args...)
}

func (r *cliReporter) Progress(msg string) {
	if msg != r.lastMsg {
		if r.lastMsg != "" {
			fmt.Println() // Finish the previous progress line
		}
		fmt.Print(msg, " ")
		r.lastMsg = msg
	} else {
		fmt.Print(".")
	}
}

func (r *cliReporter) ProgressDone(msg, status string) {
	if msg != r.lastMsg {
		// Shouldn't happen ideally, but handle it gracefully
		if r.lastMsg != "" {
			fmt.Println()
		}
		fmt.Print(msg, " ")
	}
	fmt.Printf(" %s.\n", status)
	r.lastMsg = ""
}

func main() {
	var (
		baseDir = flag.String("dir", "_mirror", "Directory to store the mirrored data")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] <owner/repo>...\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	repos := flag.Args()
	if len(repos) == 0 {
		flag.Usage()
		os.Exit(1)
	}

	token := getToken()

	reporter := &cliReporter{}
	client := github.NewClient(nil, token, reporter)

	hasError := false
	for _, repoPath := range repos {
		parts := strings.SplitN(repoPath, "/", 2)
		if len(parts) != 2 {
			fmt.Fprintf(os.Stderr, "Error: invalid repository format %q. Expected \"owner/repo\".\n", repoPath)
			hasError = true
			continue
		}
		owner, repo := parts[0], parts[1]

		if err := ghsync.Sync(client, owner, repo, *baseDir, reporter); err != nil {
			fmt.Fprintf(os.Stderr, "Error syncing %s/%s: %v\n", owner, repo, err)
			if isAuthError(err) {
				printAuthHint()
			}
			hasError = true
			continue // Continue to the next repo if one fails
		}
	}

	if hasError {
		fmt.Fprintln(os.Stderr, "Sync completed with errors.")
		os.Exit(1)
	}
}
