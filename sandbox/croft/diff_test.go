//go:build linux

package croft

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	trace "github.com/buchenberg/goatherd-kernel"
	sbx "github.com/buchenberg/goatherd-runtime/sandbox"

	croftlib "github.com/buchenberg/goatherd-croft"
)

// capturedState drives Create+Capture so a Diff has a real state to compare
// against, and returns the lifted kernel state.
func capturedState(t *testing.T, s *Sandbox) trace.WorkspaceState {
	t.Helper()
	ctx := context.Background()
	if err := s.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ws, err := s.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	return ws
}

func TestSandbox_DiffBridgesBothCroftCalls(t *testing.T) {
	// The runtime's signature joins what croft splits: the unified text comes
	// from Diff, the changed-path list from Changes. Both must be consulted, or
	// one half of the result is invented.
	s, fake := newTestSandbox(t)
	ws := capturedState(t, s)

	fake.mu.Lock()
	fake.diffText = "--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-one\n+two\n"
	fake.changes = []croftlib.Change{
		{Path: "a.txt", Kind: croftlib.ChangeModify, Size: 3},
		{Path: "new.txt", Kind: croftlib.ChangeAdd, Size: 5},
	}
	fake.mu.Unlock()

	diff, files, err := s.Diff(context.Background(), ws, 0)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if diff != fake.diffText {
		t.Errorf("diff text = %q, want croft's Diff output %q", diff, fake.diffText)
	}
	if strings.Join(files, ",") != "a.txt,new.txt" {
		t.Errorf("files = %v, want [a.txt new.txt] from croft's Changes", files)
	}
	if got := strings.Join(fake.calls, ","); !strings.Contains(got, "changes") || !strings.Contains(got, "diff") {
		t.Errorf("Diff must consult both croft verbs, calls were: %s", got)
	}
}

func TestSandbox_DiffDropsDirectories(t *testing.T) {
	// `git diff --name-only` never lists a directory, so neither may this
	// backend: a caller comparing paths against a proposal's touched-file set
	// should not have to know which world produced the list.
	s, fake := newTestSandbox(t)
	ws := capturedState(t, s)

	fake.mu.Lock()
	fake.changes = []croftlib.Change{
		{Path: "pkg", Kind: croftlib.ChangeAdd, IsDir: true},
		{Path: "pkg/a.go", Kind: croftlib.ChangeAdd, Size: 10},
		{Path: "pkg/b.go", Kind: croftlib.ChangeModify, Size: 12},
	}
	fake.diffText = "diff"
	fake.mu.Unlock()

	_, files, err := s.Diff(context.Background(), ws, 0)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if strings.Join(files, ",") != "pkg/a.go,pkg/b.go" {
		t.Errorf("files = %v, want only the non-directory paths", files)
	}
}

func TestSandbox_DiffKeepsDeletesAndSymlinks(t *testing.T) {
	// A delete has no size and a deleted symlink is identified by its mode bit,
	// not IsDir — parsing names back out of the diff text would misreport both,
	// which is why the file list comes from Changes.
	s, fake := newTestSandbox(t)
	ws := capturedState(t, s)

	fake.mu.Lock()
	fake.changes = []croftlib.Change{
		{Path: "gone.txt", Kind: croftlib.ChangeDelete},
		{Path: "link", Kind: croftlib.ChangeDelete, Mode: fs.ModeSymlink | 0o777},
	}
	fake.diffText = "diff"
	fake.mu.Unlock()

	_, files, err := s.Diff(context.Background(), ws, 0)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if strings.Join(files, ",") != "gone.txt,link" {
		t.Errorf("files = %v, want both deleted paths including the symlink", files)
	}
}

func TestSandbox_DiffTruncatesTextOnly(t *testing.T) {
	// maxLines caps the diff text but never the file list: the paths are what a
	// caller iterates to decide whether a proposal touches anything unexpected,
	// and silently shortening that would hide a change.
	s, fake := newTestSandbox(t)
	ws := capturedState(t, s)

	fake.mu.Lock()
	fake.diffText = "line1\nline2\nline3\nline4\nline5\n"
	fake.changes = []croftlib.Change{
		{Path: "a", Kind: croftlib.ChangeAdd},
		{Path: "b", Kind: croftlib.ChangeAdd},
		{Path: "c", Kind: croftlib.ChangeAdd},
	}
	fake.mu.Unlock()

	diff, files, err := s.Diff(context.Background(), ws, 2)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.HasSuffix(diff, truncatedMarker) {
		t.Errorf("truncated diff must end with the marker, got %q", diff)
	}
	if got := strings.Count(diff, "\n"); got > 3 {
		t.Errorf("diff was not truncated to 2 lines plus the marker: %q", diff)
	}
	if len(files) != 3 {
		t.Errorf("the file list must survive truncation intact, got %v", files)
	}
	// The marker matches the git backend's, so callers see one sentinel.
	if truncatedMarker != "...[diff truncated]" {
		t.Errorf("marker %q drifted from the git backend's", truncatedMarker)
	}
}

func TestSandbox_DiffUnboundedWhenMaxLinesNonPositive(t *testing.T) {
	s, fake := newTestSandbox(t)
	ws := capturedState(t, s)

	long := strings.Repeat("x\n", 500)
	fake.mu.Lock()
	fake.diffText = long
	fake.mu.Unlock()

	for _, maxLines := range []int{0, -1} {
		diff, _, err := s.Diff(context.Background(), ws, maxLines)
		if err != nil {
			t.Fatalf("Diff(maxLines=%d): %v", maxLines, err)
		}
		if diff != long {
			t.Errorf("maxLines=%d must mean unbounded; the text was altered", maxLines)
		}
		if strings.Contains(diff, truncatedMarker) {
			t.Errorf("maxLines=%d must not truncate", maxLines)
		}
	}
}

func TestSandbox_DiffShorterThanMaxLinesIsUntouched(t *testing.T) {
	s, fake := newTestSandbox(t)
	ws := capturedState(t, s)

	fake.mu.Lock()
	fake.diffText = "one\ntwo\n"
	fake.mu.Unlock()

	diff, _, err := s.Diff(context.Background(), ws, 100)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if diff != "one\ntwo\n" {
		t.Errorf("a diff under the limit must pass through byte-for-byte, got %q", diff)
	}
}

func TestSandbox_DiffEmptyChangeSet(t *testing.T) {
	// A clean workspace yields an empty file list, not a nil that a caller
	// ranging over it would still handle but whose length reads ambiguously.
	s, fake := newTestSandbox(t)
	ws := capturedState(t, s)

	fake.mu.Lock()
	fake.changes = []croftlib.Change{}
	fake.diffText = ""
	fake.mu.Unlock()

	diff, files, err := s.Diff(context.Background(), ws, 0)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if diff != "" || len(files) != 0 {
		t.Errorf("clean workspace: diff = %q, files = %v; want both empty", diff, files)
	}
}

func TestSandbox_DiffRejectsForeignBackend(t *testing.T) {
	s, _ := newTestSandbox(t)
	_, _, err := s.Diff(context.Background(), trace.WorkspaceState{Backend: "git", Revision: "abc"}, 0)
	if err == nil {
		t.Fatal("Diff must reject a state from another backend")
	}
}

func TestSandbox_DiffPropagatesErrors(t *testing.T) {
	// Each half of the bridge can fail independently, and both must surface —
	// a swallowed Changes error would return a diff whose file list is empty
	// rather than unknown, which reads as "nothing changed".
	t.Run("changes", func(t *testing.T) {
		s, fake := newTestSandbox(t)
		ws := capturedState(t, s)
		fake.mu.Lock()
		fake.changesErr = croftlib.ErrStateUnavailable
		fake.mu.Unlock()

		_, _, err := s.Diff(context.Background(), ws, 0)
		if err == nil {
			t.Fatal("a Changes failure must surface")
		}
		if !errors.Is(err, croftlib.ErrStateUnavailable) {
			t.Errorf("the sentinel must survive wrapping: %v", err)
		}
	})

	t.Run("diff", func(t *testing.T) {
		s, fake := newTestSandbox(t)
		ws := capturedState(t, s)
		fake.mu.Lock()
		fake.diffErr = errors.New("daemon read failed")
		fake.mu.Unlock()

		if _, _, err := s.Diff(context.Background(), ws, 0); err == nil {
			t.Fatal("a Diff failure must surface")
		}
	})
}
