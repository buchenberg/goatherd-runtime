package goatherd

// Fork options: explicit baseline, seeded sandbox, snapshot persistence, and
// ForkFromOutput. These pin the goatherd-runtime child "explicit
// Baseline/Seed/Snapshot in ForkIsolated (ForkFromOutput)".

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	trace "github.com/buchenberg/goatherd-kernel"
	sandbox "github.com/buchenberg/goatherd-runtime/sandbox"
	"github.com/buchenberg/goatherd-runtime/sandbox/git"
)

// newForkWorktree provisions a worktree sandbox for a fork test and returns
// the sandbox plus the path the test reads and writes.
func newForkWorktree(t *testing.T, repo, leaf string) (sandbox.Sandbox, string) {
	t.Helper()
	wtPath := filepath.Join(t.TempDir(), leaf)
	sb := git.NewWorktreeSandbox(repo, wtPath)
	if err := sb.Create(context.Background(), sandbox.SandboxSpec{}); err != nil {
		t.Fatalf("worktree create: %v", err)
	}
	return sb, wtPath
}

// recoverForkScopes reopens the store and recovers with a resolver that maps
// each isolated child to its recorded worktree path and every root to repo.
func recoverForkScopes(t *testing.T, store *trace.SQLiteTraceStore, path, repo string, worktrees map[string]string) *ScopeManager {
	t.Helper()
	recovered, _ := recoverForkScopesWithStore(t, store, path, repo, worktrees)
	return recovered
}

// recoverForkScopesWithStore is recoverForkScopes plus the reopened store, for
// tests that inspect records after recovery.
func recoverForkScopesWithStore(t *testing.T, store *trace.SQLiteTraceStore, path, repo string, worktrees map[string]string) (*ScopeManager, *trace.SQLiteTraceStore) {
	t.Helper()
	reopened := reopenStore(t, store, path)
	recovered, err := RecoverScopes(reopened, RecoverOptions{
		Resolver: func(scopeID, backend, addr string) (sandbox.Sandbox, error) {
			if wt, ok := worktrees[scopeID]; ok {
				return git.NewWorktreeSandbox(repo, wt), nil
			}
			return git.NewLocalGitSandbox(repo), nil
		},
	})
	if err != nil {
		t.Fatalf("RecoverScopes: %v", err)
	}
	return recovered, reopened
}

func digestOf(t *testing.T, ws trace.WorkspaceState) string {
	t.Helper()
	digest, err := ws.Digest()
	if err != nil {
		t.Fatalf("workspace digest: %v", err)
	}
	return digest
}

// TestForkOptions_SeedAgainstDifferentBaseline is the acceptance case: a child
// seeded from a sealed output but measured against the parent's current tree
// carries the seeded file as its delta.
func TestForkOptions_SeedAgainstDifferentBaseline(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, err := mgr.Create("forkseed:parent", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	src, srcWt := forkWorktreeChild(t, mgr, parent.ID(), "forkseed:source", repo, "wt-src")
	if err := os.WriteFile(filepath.Join(srcWt, "out.txt"), []byte("from output\n"), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}
	out, err := mgr.Seal(ctx, src.ID())
	if err != nil {
		t.Fatalf("seal source: %v", err)
	}

	parentState, err := parent.Sandbox().Capture(ctx)
	if err != nil {
		t.Fatalf("capture parent: %v", err)
	}

	sb, wt := newForkWorktree(t, repo, "wt-seed")
	child, err := mgr.ForkIsolated(parent.ID(), "forkseed:child", nil, sb, ForkOptions{Seed: out.Workspace()})
	if err != nil {
		t.Fatalf("ForkIsolated with seed: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(wt, "out.txt"))
	if err != nil {
		t.Fatalf("read seeded file: %v", err)
	}
	if string(got) != "from output\n" {
		t.Errorf("seeded file = %q, want %q", got, "from output\n")
	}

	baseline, ok := child.Baseline()
	if !ok {
		t.Fatal("child has no baseline")
	}
	if digestOf(t, baseline) != digestOf(t, parentState) {
		t.Error("baseline is not the parent's current tree")
	}
	if digestOf(t, baseline) == digestOf(t, out.Workspace()) {
		t.Error("baseline was taken from the seed instead of the parent")
	}

	// The seeded file is the child's delta against the parent baseline.
	sealed, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("seal seeded child: %v", err)
	}
	if len(sealed.Changes()) != 1 {
		t.Fatalf("changes = %+v, want exactly out.txt", sealed.Changes())
	}
	if c := sealed.Changes()[0]; c.Path != "out.txt" || c.Kind != ChangeCreate {
		t.Errorf("change = %+v, want create of out.txt", c)
	}
}

// TestForkFromOutput_FrozenBaselineAndSnapshot pins that the convenience uses
// the output's frozen state for both seed and baseline: the parent moving
// after the seal cannot leak into the child's delta, and the snapshot comes
// from the output.
func TestForkFromOutput_FrozenBaselineAndSnapshot(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, err := mgr.Create("forkout:parent", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	src, srcWt := forkWorktreeChild(t, mgr, parent.ID(), "forkout:source", repo, "wt-src")
	if err := os.WriteFile(filepath.Join(srcWt, "out.txt"), []byte("sealed content\n"), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}
	snapshot := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out, err := mgr.Seal(ctx, src.ID(), SealOptions{Snapshot: snapshot})
	if err != nil {
		t.Fatalf("seal source: %v", err)
	}

	// Move the parent after the seal. The fork must not observe this.
	if err := os.WriteFile(filepath.Join(repo, "after.txt"), []byte("later\n"), 0o644); err != nil {
		t.Fatalf("write parent file: %v", err)
	}

	sb, wt := newForkWorktree(t, repo, "wt-fromout")
	child, err := mgr.ForkFromOutput(ctx, parent.ID(), "forkout:child", out, sb)
	if err != nil {
		t.Fatalf("ForkFromOutput: %v", err)
	}

	if got := child.Snapshot(); !reflect.DeepEqual(got, snapshot) {
		t.Errorf("child Snapshot = %v, want the output's %v", got, snapshot)
	}
	baseline, ok := child.Baseline()
	if !ok {
		t.Fatal("child has no baseline")
	}
	if digestOf(t, baseline) != digestOf(t, out.Workspace()) {
		t.Error("baseline is not the output's frozen workspace")
	}
	if got, err := os.ReadFile(filepath.Join(wt, "out.txt")); err != nil || string(got) != "sealed content\n" {
		t.Errorf("seeded file = %q, %v; want the output's content", got, err)
	}

	// The child's tree equals its frozen baseline: the parent's later file
	// is not part of the delta.
	sealed, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("seal child: %v", err)
	}
	if len(sealed.Changes()) != 0 {
		t.Errorf("changes = %+v, want none", sealed.Changes())
	}
}

// TestForkFromOutput_SnapshotSurvivesRestart covers the recovery half of the
// acceptance: the fork record restores the snapshot the output supplied.
func TestForkFromOutput_SnapshotSurvivesRestart(t *testing.T) {
	store, path := fileStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, err := mgr.Create("forkrestart:parent", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	src, srcWt := forkWorktreeChild(t, mgr, parent.ID(), "forkrestart:source", repo, "wt-src")
	if err := os.WriteFile(filepath.Join(srcWt, "out.txt"), []byte("carry me\n"), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}
	snapshot := []byte{0x00, 0x01, 0xfe, 0xff}
	out, err := mgr.Seal(ctx, src.ID(), SealOptions{Snapshot: snapshot})
	if err != nil {
		t.Fatalf("seal source: %v", err)
	}

	sb, wt := newForkWorktree(t, repo, "wt-restart")
	child, err := mgr.ForkFromOutput(ctx, parent.ID(), "forkrestart:child", out, sb)
	if err != nil {
		t.Fatalf("ForkFromOutput: %v", err)
	}
	if got := child.Snapshot(); !bytes.Equal(got.([]byte), snapshot) {
		t.Fatalf("live snapshot = %v, want %v", got, snapshot)
	}

	recovered := recoverForkScopes(t, store, path, repo, map[string]string{
		"scope:forkrestart:source": srcWt,
		"scope:forkrestart:child":  wt,
	})
	gotScope, ok := recovered.Get(child.ID())
	if !ok {
		t.Fatal("child not recovered")
	}
	got, ok := gotScope.Snapshot().([]byte)
	if !ok {
		t.Fatalf("recovered snapshot type = %T, want []byte", gotScope.Snapshot())
	}
	if !bytes.Equal(got, snapshot) {
		t.Errorf("recovered snapshot = %v, want %v", got, snapshot)
	}
	if base, ok := gotScope.Baseline(); !ok || digestOf(t, base) != digestOf(t, out.Workspace()) {
		t.Error("recovered baseline is not the output's frozen workspace")
	}
}

// TestFork_SnapshotSurvivesRecovery covers shared forks and the two snapshot
// encodings: strings/maps as normalized JSON, []byte as base64.
func TestFork_SnapshotSurvivesRecovery(t *testing.T) {
	cases := []struct {
		name     string
		snapshot any
	}{
		{"string", "conversation-state"},
		{"map", map[string]any{"role": "user", "content": "hi"}},
		{"bytes", []byte{0x00, 0x7f, 0x80, 0xff}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, path := fileStore(t)
			mgr := NewScopeManager(store)
			repo := newTestRepo(t)
			parent, err := mgr.Create("forksnap:parent-"+tc.name, git.NewLocalGitSandbox(repo))
			if err != nil {
				t.Fatalf("create parent: %v", err)
			}
			child, err := mgr.Fork(parent.ID(), "forksnap:child-"+tc.name, tc.snapshot)
			if err != nil {
				t.Fatalf("Fork: %v", err)
			}

			recovered := recoverForkScopes(t, store, path, repo, nil)
			gotScope, ok := recovered.Get(child.ID())
			if !ok {
				t.Fatal("child not recovered")
			}
			if got := gotScope.Snapshot(); !reflect.DeepEqual(got, tc.snapshot) {
				t.Errorf("recovered snapshot = %#v, want %#v", got, tc.snapshot)
			}
		})
	}
}

func TestFork_SeedRequiresIsolatedSandbox(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, err := mgr.Create("forkbadseed:parent", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	state, err := parent.Sandbox().Capture(ctx)
	if err != nil {
		t.Fatalf("capture parent: %v", err)
	}

	if _, err := mgr.Fork(parent.ID(), "forkbadseed:shared", nil, ForkOptions{Seed: state}); err == nil || !strings.Contains(err.Error(), "isolated fork") {
		t.Errorf("shared Fork with seed error = %v, want the isolated-sandbox refusal", err)
	}
	if _, err := parent.Fork("forkbadseed:direct", nil, ForkOptions{Seed: state}); err == nil {
		t.Error("Scope.Fork accepted a seed")
	}
}

func TestFork_SeedDoesNotMutateOnDuplicateOwner(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, err := mgr.Create("forkdup:parent", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	src, srcWt := forkWorktreeChild(t, mgr, parent.ID(), "forkdup:source", repo, "wt-src")
	if err := os.WriteFile(filepath.Join(srcWt, "out.txt"), []byte("seed content\n"), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}
	out, err := mgr.Seal(ctx, src.ID())
	if err != nil {
		t.Fatalf("seal source: %v", err)
	}

	// Occupy the owner first; the duplicate fork below must fail without
	// touching the second sandbox.
	forkWorktreeChild(t, mgr, parent.ID(), "forkdup:child", repo, "wt-first")

	sb, wt := newForkWorktree(t, repo, "wt-dup")
	if _, err := mgr.ForkIsolated(parent.ID(), "forkdup:child", nil, sb, ForkOptions{Seed: out.Workspace()}); err == nil {
		t.Fatal("duplicate fork succeeded")
	}
	if _, err := os.Stat(filepath.Join(wt, "out.txt")); !os.IsNotExist(err) {
		t.Errorf("duplicate fork seeded the sandbox anyway: %v", err)
	}
}

// applyFailSandbox fails Apply so the seed path can be tested without a
// backend that can be made to fail deterministically.
type applyFailSandbox struct{ sandbox.Sandbox }

func (applyFailSandbox) Apply(context.Context, trace.WorkspaceState) error {
	return errors.New("apply refused by test sandbox")
}

func TestFork_SeedFailureLeavesNoGhostChild(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	parent, err := mgr.Create("forkfail:parent", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	state, err := parent.Sandbox().Capture(context.Background())
	if err != nil {
		t.Fatalf("capture parent: %v", err)
	}
	sb, _ := newForkWorktree(t, repo, "wt-fail")
	if _, err := mgr.ForkIsolated(parent.ID(), "forkfail:child", nil, applyFailSandbox{sb}, ForkOptions{Seed: state}); err == nil {
		t.Fatal("fork succeeded despite a failing seed")
	}
	if _, ok := mgr.Get("scope:forkfail:child"); ok {
		t.Error("failed seed registered a child")
	}
	if forks := findRecords(t, store, "forkfail:parent", SchemaScopeForked); len(forks) != 0 {
		t.Errorf("failed seed left %d fork records, want 0", len(forks))
	}
}

func TestForkFromOutput_NoSnapshotStaysNil(t *testing.T) {
	store, path := fileStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, err := mgr.Create("forknil:parent", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	src, srcWt := forkWorktreeChild(t, mgr, parent.ID(), "forknil:source", repo, "wt-src")
	if err := os.WriteFile(filepath.Join(srcWt, "out.txt"), []byte("no snapshot\n"), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}
	out, err := mgr.Seal(ctx, src.ID()) // deliberately no snapshot
	if err != nil {
		t.Fatalf("seal source: %v", err)
	}

	sb, wt := newForkWorktree(t, repo, "wt-nil")
	child, err := mgr.ForkFromOutput(ctx, parent.ID(), "forknil:child", out, sb)
	if err != nil {
		t.Fatalf("ForkFromOutput: %v", err)
	}
	if got := child.Snapshot(); got != nil {
		t.Errorf("live Snapshot = %#v, want nil for a snapshot-less output", got)
	}

	forks := findRecords(t, store, "forknil:parent", SchemaScopeForked)
	var payload map[string]any
	for _, f := range forks {
		if f.Body.Payload["child_owner"] == "forknil:child" {
			payload = f.Body.Payload
		}
	}
	if payload == nil {
		t.Fatal("no fork record for the child")
	}
	if _, ok := payload["snapshot"]; ok {
		t.Error("snapshot-less output recorded a snapshot entry")
	}

	recovered := recoverForkScopes(t, store, path, repo, map[string]string{
		"scope:forknil:source": srcWt,
		"scope:forknil:child":  wt,
	})
	gotScope, ok := recovered.Get(child.ID())
	if !ok {
		t.Fatal("child not recovered")
	}
	if got := gotScope.Snapshot(); got != nil {
		t.Errorf("recovered Snapshot = %#v, want nil", got)
	}
}

func TestFork_NilByteSnapshotStaysAbsent(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	parent, err := mgr.Create("forknilbytes:parent", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child, err := mgr.Fork(parent.ID(), "forknilbytes:child", nil, ForkOptions{Snapshot: []byte(nil)})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if got := child.Snapshot(); got != nil {
		t.Errorf("Snapshot = %#v, want nil for a typed-nil []byte", got)
	}
	forks := findRecords(t, store, "forknilbytes:parent", SchemaScopeForked)
	if len(forks) != 1 {
		t.Fatalf("fork records = %d, want 1", len(forks))
	}
	if _, ok := forks[0].Body.Payload["snapshot"]; ok {
		t.Error("typed-nil []byte recorded a snapshot entry")
	}
}

func TestForkSnapshotFromPayload_NonStringBase64IsAbsent(t *testing.T) {
	if got := forkSnapshotFromPayload(map[string]any{"snapshot": 42, "snapshot_encoding": "base64"}); got != nil {
		t.Errorf("non-string base64 payload = %#v, want nil", got)
	}
	want := []byte("bytes")
	got, ok := forkSnapshotFromPayload(map[string]any{
		"snapshot":          base64.StdEncoding.EncodeToString(want),
		"snapshot_encoding": "base64",
	}).([]byte)
	if !ok || !bytes.Equal(got, want) {
		t.Errorf("valid base64 payload = %#v, want %v", got, want)
	}
}

func TestFork_RejectsMultipleOptionSets(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	parent, err := mgr.Create("forkmulti:parent", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	if _, err := mgr.Fork(parent.ID(), "forkmulti:child", nil, ForkOptions{}, ForkOptions{}); err == nil {
		t.Fatal("Fork accepted two option sets")
	}
}
