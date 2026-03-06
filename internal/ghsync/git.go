// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ghsync

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// commitChanges initializes a git repository if necessary and commits all changes.
func (s *syncer) commitChanges() error {
	if err := os.MkdirAll(s.rootDir, 0755); err != nil {
		return fmt.Errorf("creating root directory: %w", err)
	}

	// Initialize git repo if it doesn't exist
	cmd := exec.Command("git", "init")
	cmd.Dir = s.rootDir
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git init: %w", err)
	}

	repoPath := filepath.Join(s.owner, s.repo)

	// Add all changes for this specific repository
	cmd = exec.Command("git", "add", repoPath)
	cmd.Dir = s.rootDir
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git add: %w", err)
	}

	// Check if there are any changes to commit
	cmd = exec.Command("git", "status", "--porcelain", repoPath)
	cmd.Dir = s.rootDir
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git status: %w", err)
	}

	if len(output) == 0 {
		s.reporter.Logf("No new changes to commit.\n")
		return nil
	}

	// Commit changes
	commitMsg := fmt.Sprintf("Sync %s/%s up to %s", s.owner, s.repo, time.Now().UTC().Format(time.RFC3339))
	cmd = exec.Command("git", "commit", "--no-verify", "-m", commitMsg)
	cmd.Dir = s.rootDir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=ghmirror",
		"GIT_AUTHOR_EMAIL=ghmirror@localhost",
		"GIT_COMMITTER_NAME=ghmirror",
		"GIT_COMMITTER_EMAIL=ghmirror@localhost",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git commit: %w\nOutput: %s", err, string(out))
	}

	s.reporter.Logf("Committed changes: %s\n", commitMsg)

	return nil
}
