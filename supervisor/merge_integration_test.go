package supervisor_test

// Cross-component integration test: the supervisor's rule engine wired into
// the merge gate's reviewer (plan 04 §3, T3.4). It lives with the supervisor
// because it exercises the rule engine and the scope tree together through
// the public surface.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	trace "github.com/buchenberg/goatherd-kernel"
	goatherd "github.com/buchenberg/goatherd-runtime"
	"github.com/buchenberg/goatherd-runtime/internal/gittest"
	"github.com/buchenberg/goatherd-runtime/internal/rttest"
	"github.com/buchenberg/goatherd-runtime/sandbox/git"
	"github.com/buchenberg/goatherd-runtime/supervisor"
)

// withinDrafts mirrors the merge gate's drafts-prefix rule: prefix itself or
// inside prefix/.
func withinDrafts(p string) bool {
	return p == "drafts" || strings.HasPrefix(p, "drafts/")
}

// TestSupervisor_ReviewMerge covers T3.4: rule engines get merge proposals as
// events, supervisor.InterventionDiscard gains a live emitter, and wiring the
// verdict into the explicit proposal/decision pair denies without merging.

// fileExists reports whether a path exists in the test repo.
func fileExists(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func TestSupervisor_ReviewMerge(t *testing.T) {
	store := rttest.NewMemStore(t)
	mgr := goatherd.NewScopeManager(store)
	repo := gittest.NewTestRepo(t)
	parent, _ := mgr.Create("gate:rev-parent", git.NewLocalGitSandbox(repo))

	sup := supervisor.NewSupervisor(mgr, nil) // CheckCall-only: no bus needed
	sup.AddRule(supervisor.SupervisionRule{
		Name: "drafts_only_merge_gate",
		Match: func(e trace.EffectEvent) bool {
			return e.SchemaRef == goatherd.SchemaMergeProposed
		},
		Action: func(e trace.EffectEvent) *supervisor.Intervention {
			paths, _ := e.Payload["paths"].([]string)
			for _, p := range paths {
				if !withinDrafts(p) {
					return &supervisor.Intervention{
						Type:    supervisor.InterventionDiscard,
						ScopeID: "scope:" + e.TraceOwnerID,
						Payload: "changes must stay under drafts/",
						Time:    time.Now(),
					}
				}
			}
			return nil
		},
	})

	reviewer := func(p goatherd.MergeProposal) error {
		if iv := sup.ReviewMerge(p.ChildScopeID, p); iv != nil && iv.Type == supervisor.InterventionDiscard {
			return errors.New(fmt.Sprint(iv.Payload))
		}
		return nil
	}

	// A shared-sandbox child writes outside the prefix, and the supervisor's
	// rule denies the merge through ReviewMerge — the emitter story in one flow.
	child, err := mgr.Fork(parent.ID(), "gate:rev-child", nil)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "elsewhere.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	proposal, err := mgr.ProposeMerge(context.Background(), child.ID(), 100)
	if err != nil {
		t.Fatalf("ProposeMerge: %v", err)
	}
	proposedID, err := mgr.RecordMergeProposal(context.Background(), proposal)
	if err != nil {
		t.Fatalf("RecordMergeProposal: %v", err)
	}
	reviewErr := reviewer(*proposal)
	if reviewErr == nil {
		t.Fatal("ReviewMerge should have denied the out-of-prefix child")
	}
	if _, err := mgr.RecordMergeDecision(context.Background(), proposal, false, reviewErr.Error(), "discarded", proposedID); err != nil {
		t.Fatalf("RecordMergeDecision: %v", err)
	}
	if err := mgr.Discard(child.ID()); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if !strings.Contains(reviewErr.Error(), "drafts/") {
		t.Errorf("denial text = %q, want the rule's payload as the reason", reviewErr.Error())
	}
	if child.State() != goatherd.ScopeDiscarded {
		t.Errorf("child state = %s, want discarded", child.State())
	}
	// A denial is causal-only: a shared-sandbox child's write is already in
	// the parent tree, so Discard removes nothing from disk — the caller's
	// checkpoint restore is the physical rollback. Assert the persistence
	// explicitly rather than logging a dead "file gone" branch.
	if !fileExists(t, repo, "elsewhere.txt") {
		t.Error("elsewhere.txt vanished: a denied shared-sandbox merge must leave the parent tree untouched")
	}

	// An in-prefix proposal produces no intervention (approval).
	good := goatherd.MergeProposal{
		ChildScopeID: "scope:gate:rev-child",
		Changes:      []goatherd.ProposedChange{{Path: "drafts/ok.txt", Kind: goatherd.ChangeCreate}},
	}
	if iv := sup.ReviewMerge(child.ID(), good); iv != nil {
		t.Errorf("in-prefix proposal: intervention = %+v, want nil", iv)
	}
}
