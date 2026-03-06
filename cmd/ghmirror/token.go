// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/aclements/ghmirror/internal/github"
)

func tokenFilePath() string {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(configDir, "ghmirror", "github.tok")
}

func getToken() string {
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		return token
	}

	tokFile := tokenFilePath()
	if tokFile != "" {
		b, err := os.ReadFile(tokFile)
		if err == nil {
			return strings.TrimSpace(string(b))
		}
	}

	return promptForToken(tokFile)
}

func promptForToken(tokFile string) string {
	fmt.Fprintln(os.Stderr, "GitHub Personal Access Token not found.")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "To mirror a repository efficiently, you must provide a GitHub token.")
	fmt.Fprintln(os.Stderr, "You can create a fine-grained token at:")
	fmt.Fprintln(os.Stderr, "  https://github.com/settings/personal-access-tokens/new")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "To set it up:")
	fmt.Fprintln(os.Stderr, "  - Under \"Repository access\", select \"Public Repositories (read-only)\" ")
	fmt.Fprintln(os.Stderr, "    if you only need public data (like golang/go).")
	fmt.Fprintln(os.Stderr, "  - OR, if selecting specific repositories, expand \"Repository permissions\"")
	fmt.Fprintln(os.Stderr, "    and grant Read-only access to \"Issues\".")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Alternatively, you can set the GITHUB_TOKEN environment variable.")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprint(os.Stderr, "Paste your GitHub token: ")

	bytePassword, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading token: %v\n", err)
		os.Exit(1)
	}

	token := strings.TrimSpace(string(bytePassword))
	if token == "" {
		fmt.Fprintln(os.Stderr, "Error: empty token provided.")
		os.Exit(1)
	}

	if tokFile != "" {
		if err := os.MkdirAll(filepath.Dir(tokFile), 0700); err == nil {
			if err := os.WriteFile(tokFile, []byte(token), 0600); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to save token to %s: %v\n", tokFile, err)
			} else {
				fmt.Fprintf(os.Stderr, "Token saved to %s\n\n", tokFile)
			}
		}
	}

	return token
}

func isAuthError(err error) bool {
	var apiErr *github.APIError
	if errors.As(err, &apiErr) {
		return apiErr.IsAuthError()
	}
	return false
}

func printAuthHint() {
	fmt.Fprintln(os.Stderr, "Hint: This appears to be an authentication failure.")
	if os.Getenv("GITHUB_TOKEN") != "" {
		fmt.Fprintln(os.Stderr, "The tool is currently using the token from your GITHUB_TOKEN environment variable.")
	} else {
		path := tokenFilePath()
		fmt.Fprintf(os.Stderr, "The tool is currently using the token stored at: %s\n", path)
		fmt.Fprintf(os.Stderr, "To replace it, you can delete that file or run: echo \"new_token\" > %s\n", path)
	}
}
