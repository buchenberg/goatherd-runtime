package goatherd_test

// Regression tests for the 2026-11 arch-audit P1 fixes:
//   - shepherd-runtime-go-e1y: Halt on a merged scope must be refused, and
//     recovery must quarantine an inconsistent owner instead of failing.
//   - shepherd-runtime-go-hol: concurrent SettleSelects must not both pass
//     against an unmodified parent.

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	trace "github.com/buchenberg/goatherd-kernel"
	goatherd "github.com/buchenberg/goatherd-runtime"
	"github.com/buchenberg/goatherd-runtime/internal/gittest"
	"github.com/buchenberg/goatherd-runtime/sandbox/git"
)

// openRegressionStore opens a file-backed trace store and a manager over
// it, returning the path for reopen-and-recover tests.
func openRegressionStore(t *testing.T) (*trace.SQLiteTraceStore, *goatherd.ScopeManager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "regression.db")
	store, err := trace.NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store, goatherd.NewScopeManager(store), path
}

// Halting a merged scope is a public API reaching a terminal state twice.
// It must be refused, record nothing, and leave recovery clean.
func TestHalt_MergedScopeRefused_RecoveryClean(t *testing.T) {
	ctx := context.Background()
	store, mgr, path := openRegressionStore(t)

	parent, err := mgr.Create("sub:halt-parent", git.NewLocalGitSandbox(gittest.NewTestRepo(t)))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child, err := mgr.Fork(parent.ID(), "sub:halt-child", nil)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if err := parent.Merge(child); err != nil {
		t.Fatalf("merge: %v", err)
	}

	if err := child.Halt(); err == nil {
		t.Fatal("expected Halt on a merged scope to be refused")
	}
	if child.State() != goatherd.ScopeMerged {
		t.Fatalf("refused Halt must not change state, got %s", child.State())
	}

	// Recovery on the same trace must succeed: no halt record was written.
	if err := store.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := trace.NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close(ctx)
	if _, err := goatherd.RecoverScopes(reopened, goatherd.RecoverOptions{}); err != nil {
		t.Fatalf("RecoverScopes after refused halt: %v", err)
	}
}

// A legacy trace that somehow holds both a merge and a halt for one owner
// (the pre-fix corruption shape) must not poison recovery for everyone:
// the owner is quarantined and named in the stats.
func TestRecovery_QuarantinesInconsistentOwner(t *testing.T) {
	ctx := context.Background()
	store, mgr, path := openRegressionStore(t)

	parent, err := mgr.Create("sub:q-parent", nil)
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	sibling, err := mgr.Fork(parent.ID(), "sub:q-sibling", nil)
	if err != nil {
		t.Fatalf("fork sibling: %v", err)
	}
	if err := parent.Merge(sibling); err != nil {
		t.Fatalf("merge sibling: %v", err)
	}

	// Forge the pre-fix corruption directly: a supervisor halt beside the
	// merge record for the same owner. Halt refuses this state now, so the
	// record can only come from an older binary.
	if _, err := store.Append(ctx, trace.TrustedAppendContext, trace.AppendBatch{
		AppendIntentID: "sub:q-sibling:halt:legacy",
		Groups: []trace.AppendGroup{{
			TraceOwnerID: "sub:q-sibling",
			FactDrafts: []trace.RecordDraft{{
				Mode:      trace.Declaration,
				SchemaRef: goatherd.SchemaSupervisorHalt,
				KindLabel: "supervisor:halt",
				Payload:   map[string]any{},
			}},
		}},
	}); err != nil {
		t.Fatalf("forge halt record: %v", err)
	}
	if err := store.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := trace.NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close(ctx)

	stats := &goatherd.RecoveryStats{}
	recovered, err := goatherd.RecoverScopes(reopened, goatherd.RecoverOptions{Stats: stats})
	if err != nil {
		t.Fatalf("inconsistent owner must be quarantined, not fail recovery: %v", err)
	}
	if len(stats.Quarantined) != 1 || stats.Quarantined[0] != "sub:q-sibling" {
		t.Fatalf("expected sub:q-sibling quarantined, got %v", stats.Quarantined)
	}
	if _, ok := recovered.Get("scope:sub:q-sibling"); ok {
		t.Fatalf("quarantined owner must not be recovered as a scope")
	}
	// The healthy scope is still recovered.
	if _, ok := recovered.Get("scope:sub:q-parent"); !ok {
		t.Fatalf("healthy parent missing from recovery")
	}
}

// Two concurrent SettleSelects of siblings against the same parent: the
// per-parent settle lock must make the loser's digest check (taken after
// the winner's apply landed) refuse, instead of both passing against the
// unmodified parent and diverging.
func TestSettle_ConcurrentSelects_LoserRefused(t *testing.T) {
	ctx := context.Background()
	_, mgr, _ := openRegressionStore(t)

	repo := gittest.NewTestRepo(t)
	gittest.WriteFile(t, repo, "main.go", "package main\n")
	gittest.MustGit(t, repo, "add", "-A")
	gittest.MustGit(t, repo, "commit", "-m", "initial")

	parent, err := mgr.Create("sub:race-parent", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	childA, wtA := forkWorktreeChild(t, mgr, parent.ID(), "sub:race-a", repo, "race-wt-a")
	childB, wtB := forkWorktreeChild(t, mgr, parent.ID(), "sub:race-b", repo, "race-wt-b")
	gittest.WriteFile(t, wtA, "a.txt", "A\n")
	gittest.WriteFile(t, wtB, "b.txt", "B\n")

	outA, err := mgr.Seal(ctx, childA.ID())
	if err != nil {
		t.Fatalf("seal a: %v", err)
	}
	outB, err := mgr.Seal(ctx, childB.ID())
	if err != nil {
		t.Fatalf("seal b: %v", err)
	}

	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = mgr.Settle(ctx, outA.ID(), goatherd.SettleSelect) }()
	go func() { defer wg.Done(); errs[1] = mgr.Settle(ctx, outB.ID(), goatherd.SettleSelect) }()
	wg.Wait()

	winners := 0
	var loserErr error
	for _, err := range errs {
		if err == nil {
			winners++
		} else {
			loserErr = err
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly one successful select, got %d: %v / %v", winners, errs[0], errs[1])
	}
	if !strings.Contains(loserErr.Error(), "parent workspace has moved") {
		t.Fatalf("loser should be refused with the moved-parent error, got: %v", loserErr)
	}
	selected := 0
	for _, st := range map[*goatherd.RetainedOutput]goatherd.OutputState{outA: outA.State(), outB: outB.State()} {
		if st == goatherd.OutputSelected {
			selected++
		}
	}
	if selected != 1 {
		t.Fatalf("expected exactly one output selected, got %d", selected)
	}
}
