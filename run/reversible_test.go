package run

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	trace "github.com/buchenberg/shepherd-kernel-go"
	shepherd "github.com/buchenberg/shepherd-runtime-go"
	"github.com/buchenberg/shepherd-runtime-go/internal/gittest"
	"github.com/buchenberg/shepherd-runtime-go/internal/rttest"
	"github.com/buchenberg/shepherd-runtime-go/sandbox"
	"github.com/buchenberg/shepherd-runtime-go/sandbox/confinement"
	"github.com/buchenberg/shepherd-runtime-go/sandbox/containment"
	"github.com/buchenberg/shepherd-runtime-go/sandbox/git"
)

func newManager(t *testing.T) *shepherd.ScopeManager {
	t.Helper()
	return shepherd.NewScopeManager(rttest.NewMemStore(t))
}

func newParent(t *testing.T, mgr *shepherd.ScopeManager, owner string) (*shepherd.Scope, string) {
	t.Helper()
	repo := gittest.NewTestRepo(t)
	parent, err := mgr.Create(owner, git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	return parent, repo
}

// worktreeFactory returns an Options.NewSandbox that allocates a fresh worktree
// per run.
func worktreeFactory(t *testing.T, repo string) func(string) (sandbox.Sandbox, error) {
	return func(string) (sandbox.Sandbox, error) {
		return git.NewWorktreeSandbox(repo, filepath.Join(t.TempDir(), "wt")), nil
	}
}

func writeIn(rc *RunContext, name, content string) error {
	return os.WriteFile(filepath.Join(rc.Capability.WorkingRoot, name), []byte(content), 0o644)
}

func TestReversible_MergeDefault(t *testing.T) {
	mgr := newManager(t)
	parent, repo := newParent(t, mgr, "run:merge-main")

	var sawIsolation string
	out, err := Reversible(context.Background(), mgr, parent, Options{
		OwnerID:    "run:merge-1",
		NewSandbox: worktreeFactory(t, repo),
	}, func(_ context.Context, rc *RunContext) (Result, error) {
		sawIsolation = rc.Capability.Isolation
		return Result{}, writeIn(rc, "hello.txt", "hi")
	})
	if err != nil {
		t.Fatalf("Reversible: %v", err)
	}
	if sawIsolation != "isolated" {
		t.Fatalf("capability isolation = %q, want isolated", sawIsolation)
	}
	if out.Isolation != "isolated" {
		t.Fatalf("outcome isolation = %q", out.Isolation)
	}
	if _, err := os.Stat(filepath.Join(repo, "hello.txt")); err != nil {
		t.Fatalf("merged file missing from the parent workspace: %v", err)
	}
}

func TestReversible_DiscardOnFailure(t *testing.T) {
	mgr := newManager(t)
	parent, repo := newParent(t, mgr, "run:fail-main")

	_, err := Reversible(context.Background(), mgr, parent, Options{
		OwnerID:    "run:fail-1",
		NewSandbox: worktreeFactory(t, repo),
	}, func(_ context.Context, rc *RunContext) (Result, error) {
		if err := writeIn(rc, "should-not-land.txt", "x"); err != nil {
			return Result{}, err
		}
		return Result{}, errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected the handler error")
	}
	if _, err := os.Stat(filepath.Join(repo, "should-not-land.txt")); !os.IsNotExist(err) {
		t.Fatal("a discarded run must not reach the parent workspace")
	}
}

func TestReversible_NonReversibleOptOut(t *testing.T) {
	mgr := newManager(t)
	parent, repo := newParent(t, mgr, "run:ground-main")

	out, err := Reversible(context.Background(), mgr, parent, Options{
		OwnerID:       "run:ground-1",
		NonReversible: true,
	}, func(_ context.Context, rc *RunContext) (Result, error) {
		if rc.Capability.Isolation != "ground" {
			return Result{}, errors.New("non-reversible run must be ground")
		}
		return Result{}, writeIn(rc, "ground.txt", "x")
	})
	if err != nil {
		t.Fatalf("Reversible: %v", err)
	}
	if out.Isolation != "ground" {
		t.Fatalf("outcome isolation = %q, want ground", out.Isolation)
	}
	if _, err := os.Stat(filepath.Join(repo, "ground.txt")); err != nil {
		t.Fatalf("a ground run writes directly to the parent: %v", err)
	}
}

func TestReversible_SealDisposition(t *testing.T) {
	mgr := newManager(t)
	parent, repo := newParent(t, mgr, "run:seal-main")

	out, err := Reversible(context.Background(), mgr, parent, Options{
		OwnerID:     "run:seal-1",
		Disposition: DispositionSeal,
		NewSandbox:  worktreeFactory(t, repo),
	}, func(_ context.Context, rc *RunContext) (Result, error) {
		return Result{}, writeIn(rc, "sealed.txt", "x")
	})
	if err != nil {
		t.Fatalf("Reversible: %v", err)
	}
	if out.Output == nil {
		t.Fatal("seal disposition must return a RetainedOutput")
	}
	if _, err := os.Stat(filepath.Join(repo, "sealed.txt")); !os.IsNotExist(err) {
		t.Fatal("sealing must not touch the parent workspace")
	}
	if err := mgr.Settle(context.Background(), out.Output.ID(), shepherd.SettleSelect); err != nil {
		t.Fatalf("caller settle: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "sealed.txt")); err != nil {
		t.Fatalf("settle should integrate the sealed output: %v", err)
	}
}

func TestReversible_RefusesWithoutBackend(t *testing.T) {
	mgr := newManager(t)
	parent, repo := newParent(t, mgr, "run:jail-main")

	_, err := Reversible(context.Background(), mgr, parent, Options{
		OwnerID:    "run:jail-1",
		Placement:  confinement.PlacementJail,
		NewSandbox: worktreeFactory(t, repo),
	}, func(_ context.Context, _ *RunContext) (Result, error) {
		return Result{}, nil
	})
	if !errors.Is(err, containment.ErrJailNotEstablished) {
		t.Fatalf("want ErrJailNotEstablished, got %v", err)
	}
	if len(parent.Children()) != 0 {
		t.Fatal("a refused jail must not allocate a run scope")
	}
}

func TestReversible_AuthorityMergeDeferred(t *testing.T) {
	mgr := newManager(t)
	parent, _ := newParent(t, mgr, "run:am-main")

	_, err := Reversible(context.Background(), mgr, parent, Options{
		OwnerID:     "run:am-1",
		Disposition: DispositionAuthorityMerge,
	}, func(_ context.Context, _ *RunContext) (Result, error) {
		return Result{}, nil
	})
	if !errors.Is(err, ErrAuthorityMergeUnsupported) {
		t.Fatalf("want ErrAuthorityMergeUnsupported, got %v", err)
	}
}

func TestReversible_NonReversibleWithSealRefused(t *testing.T) {
	mgr := newManager(t)
	parent, _ := newParent(t, mgr, "run:bad-main")

	_, err := Reversible(context.Background(), mgr, parent, Options{
		OwnerID:       "run:bad-1",
		NonReversible: true,
		Disposition:   DispositionSeal,
	}, func(_ context.Context, _ *RunContext) (Result, error) {
		return Result{}, nil
	})
	if err == nil {
		t.Fatal("non-reversible + seal must be refused")
	}
}

func TestReversible_MergeDeniedByReviewer(t *testing.T) {
	mgr := newManager(t)
	parent, repo := newParent(t, mgr, "run:review-main")

	_, err := Reversible(context.Background(), mgr, parent, Options{
		OwnerID:    "run:review-1",
		NewSandbox: worktreeFactory(t, repo),
		Reviewer: func(shepherd.MergeProposal) error {
			return errors.New("denied by policy")
		},
	}, func(_ context.Context, rc *RunContext) (Result, error) {
		return Result{}, writeIn(rc, "denied.txt", "x")
	})
	var denied *shepherd.SupervisorDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("want *SupervisorDeniedError, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "denied.txt")); !os.IsNotExist(err) {
		t.Fatal("a denied run must not reach the parent workspace")
	}
}

// fakeBackend is a containment.Backend double for the jailed-run test.
type fakeBackend struct{ available bool }

func (f *fakeBackend) Name() string                         { return "fake" }
func (f *fakeBackend) EnforcementTier() string              { return "test" }
func (f *fakeBackend) ProfileFor([]string, bool) string     { return "[]" }
func (f *fakeBackend) Probe(string, string, []string) error { return nil }
func (f *fakeBackend) Launch(string, containment.LaunchRequest) (containment.LaunchResult, error) {
	return containment.LaunchResult{ExitCode: 0}, nil
}
func (f *fakeBackend) Available() (bool, string) {
	if f.available {
		return true, "ok"
	}
	return false, "unavailable"
}

func TestReversible_HandlerRunsConfined(t *testing.T) {
	mgr := newManager(t)
	parent, repo := newParent(t, mgr, "run:conf-main")
	fake := &fakeBackend{available: true}

	out, err := Reversible(context.Background(), mgr, parent, Options{
		OwnerID:   "run:conf-1",
		Placement: confinement.PlacementJail,
		Backend:   fake,
		NewSandbox: func(string) (sandbox.Sandbox, error) {
			return git.NewWorktreeSandbox(repo, filepath.Join(t.TempDir(), "wt"),
				git.WithExec(fake, confinement.PlacementJail)), nil
		},
	}, func(ctx context.Context, rc *RunContext) (Result, error) {
		res, err := rc.Scope.Sandbox().Exec(ctx, sandbox.ExecRequest{Command: "true", AuthorityMatch: "m1"})
		if err != nil {
			return Result{}, err
		}
		return Result{Evidence: res.AuthorityEvidence}, nil
	})
	if err != nil {
		t.Fatalf("Reversible: %v", err)
	}
	if out.Result.Evidence == "" {
		t.Fatal("a jailed run must produce authority evidence")
	}
}

func TestReversible_RecordsRunOperation(t *testing.T) {
	store := rttest.NewMemStore(t)
	mgr := shepherd.NewScopeManager(store)
	repo := gittest.NewTestRepo(t)
	parent, err := mgr.Create("run:rec-main", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Reversible(context.Background(), mgr, parent, Options{
		OwnerID:    "run:rec-1",
		NewSandbox: worktreeFactory(t, repo),
	}, func(_ context.Context, rc *RunContext) (Result, error) {
		return Result{ExitCode: 0}, writeIn(rc, "x.txt", "x")
	}); err != nil {
		t.Fatalf("Reversible: %v", err)
	}

	recs, err := trace.ScanRecordsBySchema(context.Background(), store, SchemaRunExecuted)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("run records = %d, want 1", len(recs))
	}
	if recs[0].Payload["isolation"] != "isolated" || recs[0].Payload["disposition"] != "merge" {
		t.Fatalf("run record payload = %+v", recs[0].Payload)
	}
}

func TestReversible_RecordsEvidence(t *testing.T) {
	store := rttest.NewMemStore(t)
	mgr := shepherd.NewScopeManager(store)
	repo := gittest.NewTestRepo(t)
	parent, err := mgr.Create("run:ev-main", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeBackend{available: true}

	if _, err := Reversible(context.Background(), mgr, parent, Options{
		OwnerID:   "run:ev-1",
		Placement: confinement.PlacementJail,
		Backend:   fake,
		NewSandbox: func(string) (sandbox.Sandbox, error) {
			return git.NewWorktreeSandbox(repo, filepath.Join(t.TempDir(), "wt"),
				git.WithExec(fake, confinement.PlacementJail)), nil
		},
	}, func(ctx context.Context, rc *RunContext) (Result, error) {
		res, err := rc.Scope.Sandbox().Exec(ctx, sandbox.ExecRequest{Command: "true", AuthorityMatch: "m1"})
		if err != nil {
			return Result{}, err
		}
		return Result{Evidence: res.AuthorityEvidence, ExitCode: res.ExitCode, Denied: res.Denied}, nil
	}); err != nil {
		t.Fatalf("Reversible: %v", err)
	}

	recs, err := trace.ScanRecordsBySchema(context.Background(), store, SchemaRunExecuted)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Payload["authority_evidence"] == "" {
		t.Fatalf("run record must carry the authority-evidence digest: %+v", recs)
	}
}

func TestRunContext_ExecRefusesWithoutBackend(t *testing.T) {
	rc := &RunContext{Capability: containment.Capability{}, Confinement: containment.ReadOnlySpec()}
	if _, err := rc.Exec(containment.LaunchRequest{Command: []string{"true"}}); !errors.Is(err, containment.ErrJailNotEstablished) {
		t.Fatalf("want ErrJailNotEstablished, got %v", err)
	}
}

func TestRunContext_ExecConfined(t *testing.T) {
	fake := &fakeBackend{available: true}
	rc := &RunContext{
		Capability:  containment.Capability{Backend: fake, WorkingRoot: t.TempDir()},
		Confinement: containment.ReadOnlySpec(),
	}
	res, err := rc.Exec(containment.LaunchRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0", res.ExitCode)
	}
}

func TestVerifyHandler_RefusesWithoutCapability(t *testing.T) {
	mgr := newManager(t)
	parent, _ := newParent(t, mgr, "run:vh-main")
	probe := &RunContext{Scope: parent, Capability: containment.Capability{}}

	// A handler that refuses through the guard passes.
	if err := VerifyHandler(context.Background(), probe, func(_ context.Context, rc *RunContext) (Result, error) {
		return Result{}, rc.Capability.RequireAuthority()
	}); err != nil {
		t.Fatalf("execution-bound handler should pass conformance: %v", err)
	}

	// A handler that runs without authority fails.
	if err := VerifyHandler(context.Background(), probe, func(_ context.Context, _ *RunContext) (Result, error) {
		return Result{}, nil
	}); err == nil {
		t.Fatal("a handler that ran without authority must fail conformance")
	}

	// A handler that refuses with the wrong error fails.
	if err := VerifyHandler(context.Background(), probe, func(_ context.Context, _ *RunContext) (Result, error) {
		return Result{}, errors.New("unrelated failure")
	}); err == nil {
		t.Fatal("a handler refusing with the wrong error must fail conformance")
	}
}
