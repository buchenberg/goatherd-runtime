//go:build linux

package croft

import (
	"context"
	"fmt"
	"strings"

	trace "github.com/buchenberg/goatherd-kernel"

	croftlib "github.com/buchenberg/goatherd-croft"
)

// truncatedMarker matches the git backend's, so a caller truncating diffs by
// maxLines sees the same sentinel whichever world it is looking at.
const truncatedMarker = "...[diff truncated]"

// Diff returns the unified diff between ws and the current workspace, plus the
// list of changed file paths.
//
// It bridges two croft calls, because croft splits what the runtime's
// signature joins: Diff produces the text, and Changes produces the typed
// change set the file list comes from. Both are read-only against the
// workspace — neither stops the task, commits, nor writes.
//
// The two calls take croft's shared operation lock separately, so a concurrent
// Exec could land between them and make the text and the file list describe
// slightly different moments. Callers diff a quiescent workspace (settlement
// does), and closing the window would mean a lock croft does not expose; the
// alternative — parsing file names back out of the diff text — would misreport
// mode-only changes, binary files and deleted symlinks, all of which Changes
// classifies correctly. The trade is recorded here rather than hidden.
//
// maxLines truncates the diff text only, never the file list: the paths are
// what a caller iterates to decide whether a proposal touches anything
// unexpected, and silently shortening that would hide a change. A non-positive
// maxLines means unbounded.
func (s *Sandbox) Diff(ctx context.Context, ws trace.WorkspaceState, maxLines int) (string, []string, error) {
	cs, err := fromTraceState(ws)
	if err != nil {
		return "", nil, err
	}

	changes, err := s.inner.Changes(ctx, cs)
	if err != nil {
		return "", nil, fmt.Errorf("croft sandbox: diff: changes: %w", err)
	}
	text, err := s.inner.Diff(ctx, cs)
	if err != nil {
		return "", nil, fmt.Errorf("croft sandbox: diff: %w", err)
	}

	return truncateDiff(text, maxLines), changedFiles(changes), nil
}

// changedFiles extracts the non-directory paths from a change set, preserving
// croft's ordering (already sorted by Path).
//
// Directories are dropped so the result means the same thing the git backend's
// does: `git diff --name-only` never lists a directory, and a caller comparing
// paths against a proposal's touched-file set should not have to know which
// world produced it. A directory-only change is still visible in the diff text,
// and Changes remains available on this backend for a caller that wants the
// full typed set including IsDir.
func changedFiles(changes []croftlib.Change) []string {
	files := make([]string, 0, len(changes))
	for _, c := range changes {
		if c.IsDir {
			continue
		}
		files = append(files, c.Path)
	}
	return files
}

// truncateDiff caps a diff at maxLines, mirroring the git backend's cut so the
// two worlds truncate identically.
func truncateDiff(text string, maxLines int) string {
	if maxLines <= 0 {
		return text
	}
	lines := strings.Split(text, "\n")
	if len(lines) <= maxLines {
		return text
	}
	return strings.Join(lines[:maxLines], "\n") + "\n" + truncatedMarker
}
