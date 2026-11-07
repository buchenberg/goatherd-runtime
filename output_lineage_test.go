package goatherd

// Output lineage and review provenance: derived_from/supersedes edges on
// sealed and settled records, the reverse SupersededBy edge, and the
// proposal/decision pair across a restart.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/buchenberg/goatherd-runtime/sandbox/git"
)

func TestLineage_ForkFromOutputRecordsDerivedFrom(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, err := mgr.Create("lineage:parent", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	src, srcWt := forkWorktreeChild(t, mgr, parent.ID(), "lineage:source", repo, "wt-src")
	if err := os.WriteFile(filepath.Join(srcWt, "out.txt"), []byte("first output\n"), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}
	baseOut, err := mgr.Seal(ctx, src.ID())
	if err != nil {
		t.Fatalf("seal source: %v", err)
	}

	sb, _ := newForkWorktree(t, repo, "wt-next")
	next, err := mgr.ForkFromOutput(ctx, parent.ID(), "lineage:next", baseOut, sb)
	if err != nil {
		t.Fatalf("ForkFromOutput: %v", err)
	}
	if got := next.DerivedFrom(); got != baseOut.ID() {
		t.Errorf("child DerivedFrom = %q, want %q", got, baseOut.ID())
	}

	nextOut, err := mgr.Seal(ctx, next.ID())
	if err != nil {
		t.Fatalf("seal next: %v", err)
	}
	if got := nextOut.DerivedFrom(); got != baseOut.ID() {
		t.Errorf("output DerivedFrom = %q, want %q", got, baseOut.ID())
	}
	if got := nextOut.Supersedes(); got != "" {
		t.Errorf("output Supersedes = %q, want empty", got)
	}

	sealed := findRecords(t, store, "lineage:next", SchemaRunOutputSealed)
	if len(sealed) != 1 {
		t.Fatalf("sealed records: %d, want 1", len(sealed))
	}
	if got := sealed[0].Body.Payload["derived_from"]; got != baseOut.ID() {
		t.Errorf("sealed payload derived_from = %v, want %s", got, baseOut.ID())
	}

	// The fork record carries the same edge, so recovery can rebuild the
	// child's lineage even before the child seals.
	forks := findRecords(t, store, "lineage:parent", SchemaScopeForked)
	var forkPayload map[string]any
	for _, f := range forks {
		if f.Body.Payload["child_owner"] == "lineage:next" {
			forkPayload = f.Body.Payload
		}
	}
	if forkPayload == nil {
		t.Fatal("no fork record for lineage:next")
	}
	if got := forkPayload["derived_from"]; got != baseOut.ID() {
		t.Errorf("fork payload derived_from = %v, want %s", got, baseOut.ID())
	}
}

func TestLineage_SupersedesLinksBothSides(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, err := mgr.Create("lineage2:parent", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	first, firstWt := forkWorktreeChild(t, mgr, parent.ID(), "lineage2:first", repo, "wt-first")
	if err := os.WriteFile(filepath.Join(firstWt, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatalf("write first file: %v", err)
	}
	firstOut, err := mgr.Seal(ctx, first.ID())
	if err != nil {
		t.Fatalf("seal first: %v", err)
	}

	second, secondWt := forkWorktreeChild(t, mgr, parent.ID(), "lineage2:second", repo, "wt-second")
	if err := os.WriteFile(filepath.Join(secondWt, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatalf("write second file: %v", err)
	}
	secondOut, err := mgr.Seal(ctx, second.ID(), SealOptions{
		DerivedFrom: firstOut.ID(),
		Supersedes:  firstOut.ID(),
	})
	if err != nil {
		t.Fatalf("seal second: %v", err)
	}

	if got := secondOut.Supersedes(); got != firstOut.ID() {
		t.Errorf("second Supersedes = %q, want %q", got, firstOut.ID())
	}
	if got := secondOut.DerivedFrom(); got != firstOut.ID() {
		t.Errorf("second DerivedFrom = %q, want %q", got, firstOut.ID())
	}
	if got := firstOut.SupersededBy(); got != secondOut.ID() {
		t.Errorf("first SupersededBy = %q, want %q", got, secondOut.ID())
	}

	sealed := findRecords(t, store, "lineage2:second", SchemaRunOutputSealed)
	if len(sealed) != 1 {
		t.Fatalf("sealed records: %d, want 1", len(sealed))
	}
	if got := sealed[0].Body.Payload["supersedes"]; got != firstOut.ID() {
		t.Errorf("sealed payload supersedes = %v, want %s", got, firstOut.ID())
	}
	if got := sealed[0].Body.Payload["derived_from"]; got != firstOut.ID() {
		t.Errorf("sealed payload derived_from = %v, want %s", got, firstOut.ID())
	}

	// The settled record carries the same lineage, so the chain survives
	// settlement too.
	if err := mgr.Settle(ctx, secondOut.ID(), SettleRelease); err != nil {
		t.Fatalf("settle second: %v", err)
	}
	settled := findRecords(t, store, "lineage2:second", SchemaRunOutputSettled)
	if len(settled) != 1 {
		t.Fatalf("settled records: %d, want 1", len(settled))
	}
	if got := settled[0].Body.Payload["supersedes"]; got != firstOut.ID() {
		t.Errorf("settled payload supersedes = %v, want %s", got, firstOut.ID())
	}
	if got := settled[0].Body.Payload["derived_from"]; got != firstOut.ID() {
		t.Errorf("settled payload derived_from = %v, want %s", got, firstOut.ID())
	}
}

func TestLineage_UnknownSupersedesIsRefused(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, err := mgr.Create("lineage3:parent", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child, _ := forkWorktreeChild(t, mgr, parent.ID(), "lineage3:child", repo, "wt")
	if _, err := mgr.Seal(ctx, child.ID(), SealOptions{Supersedes: "out:lineage3:missing:1"}); err == nil {
		t.Fatal("seal accepted an unknown supersedes output")
	}
	if got := mgr.OutputsForScope(child.ID()); len(got) != 0 {
		t.Errorf("refused seal registered %d outputs", len(got))
	}
}

func TestLineage_SurvivesRecovery(t *testing.T) {
	store, path := fileStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, err := mgr.Create("lineage4:parent", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	src, srcWt := forkWorktreeChild(t, mgr, parent.ID(), "lineage4:source", repo, "wt-src")
	if err := os.WriteFile(filepath.Join(srcWt, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}
	firstOut, err := mgr.Seal(ctx, src.ID())
	if err != nil {
		t.Fatalf("seal source: %v", err)
	}

	sb, nextWt := newForkWorktree(t, repo, "wt-next")
	next, err := mgr.ForkFromOutput(ctx, parent.ID(), "lineage4:next", firstOut, sb)
	if err != nil {
		t.Fatalf("ForkFromOutput: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nextWt, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatalf("write next file: %v", err)
	}
	secondOut, err := mgr.Seal(ctx, next.ID(), SealOptions{Supersedes: firstOut.ID()})
	if err != nil {
		t.Fatalf("seal next: %v", err)
	}

	recovered := recoverForkScopes(t, store, path, repo, map[string]string{
		"scope:lineage4:source": srcWt,
		"scope:lineage4:next":   nextWt,
	})

	recFirst, ok := recovered.Output(firstOut.ID())
	if !ok {
		t.Fatal("first output not recovered")
	}
	recSecond, ok := recovered.Output(secondOut.ID())
	if !ok {
		t.Fatal("second output not recovered")
	}
	if got := recSecond.DerivedFrom(); got != firstOut.ID() {
		t.Errorf("recovered DerivedFrom = %q, want %q", got, firstOut.ID())
	}
	if got := recSecond.Supersedes(); got != firstOut.ID() {
		t.Errorf("recovered Supersedes = %q, want %q", got, firstOut.ID())
	}
	if got := recFirst.SupersededBy(); got != secondOut.ID() {
		t.Errorf("recovered SupersededBy = %q, want %q", got, secondOut.ID())
	}
	if got, ok := recovered.Get(next.ID()); !ok || got.DerivedFrom() != firstOut.ID() {
		t.Errorf("recovered scope lineage = %v, want %q", got, firstOut.ID())
	}
}

// TestLinkSupersededOutputs_LastInScanOrderWins pins the deterministic rule
// for multiple superseders: the last output in scan order wins.
func TestLinkSupersededOutputs_LastInScanOrderWins(t *testing.T) {
	a := &RetainedOutput{id: "out:chain:a"}
	b := &RetainedOutput{id: "out:chain:b", supersedes: a.id}
	c := &RetainedOutput{id: "out:chain:c", supersedes: a.id}

	linkSupersededOutputs([]*RetainedOutput{a, b, c})
	if a.supersededBy != c.id {
		t.Errorf("SupersededBy = %q, want the last scan-order superseder %q", a.supersededBy, c.id)
	}
	linkSupersededOutputs([]*RetainedOutput{a, c, b})
	if a.supersededBy != b.id {
		t.Errorf("SupersededBy = %q after reorder, want %q", a.supersededBy, b.id)
	}
}

// TestRecover_MergeReviewPair covers a proposal recorded before a restart:
// recovery exposes it with its fact id so the host can still record the
// decision, and the live manager reflects the decision immediately.
func TestRecover_MergeReviewPair(t *testing.T) {
	store, path := fileStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, err := mgr.Create("review:parent", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child, wt := forkWorktreeChild(t, mgr, parent.ID(), "review:child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("review me\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	proposal, err := mgr.ProposeMerge(ctx, child.ID(), 100)
	if err != nil {
		t.Fatalf("ProposeMerge: %v", err)
	}
	factID, err := mgr.RecordMergeProposal(ctx, proposal)
	if err != nil {
		t.Fatalf("RecordMergeProposal: %v", err)
	}
	if live, ok := mgr.MergeReview(child.ID()); !ok || live.Decision != nil {
		t.Fatalf("live review = %+v, %v; want an open review", live, ok)
	}

	recovered, reopened := recoverForkScopesWithStore(t, store, path, repo, map[string]string{
		"scope:review:child": wt,
	})
	review, ok := recovered.MergeReview(child.ID())
	if !ok {
		t.Fatal("review not recovered")
	}
	if review.Proposal == nil || review.Proposal.ChildScopeID != child.ID() {
		t.Fatalf("recovered proposal = %+v", review.Proposal)
	}
	if review.ProposedFactID != factID {
		t.Errorf("ProposedFactID = %q, want %q", review.ProposedFactID, factID)
	}
	if review.Decision != nil {
		t.Errorf("recovered review already has a decision: %+v", review.Decision)
	}
	if len(review.Proposal.Changes) != 1 || review.Proposal.Changes[0].Path != "feature.txt" {
		t.Errorf("recovered changes = %+v, want feature.txt", review.Proposal.Changes)
	}

	// The recovered pair is enough to record the decision after restart.
	if _, err := recovered.RecordMergeDecision(ctx, review.Proposal, true, "", "released", review.ProposedFactID); err != nil {
		t.Fatalf("RecordMergeDecision after recovery: %v", err)
	}
	after, ok := recovered.MergeReview(child.ID())
	if !ok || after.Decision == nil || !after.Decision.Approved || after.Decision.Action != "released" {
		t.Fatalf("review after decision = %+v, want approved/released", after)
	}

	// The decision record names the proposal it decides, so a later recovery
	// can join the pair without relying on scope ids alone.
	decisions := findRecords(t, reopened, "review:parent", SchemaSupervisorDecision)
	if len(decisions) != 1 {
		t.Fatalf("decision records: %d, want 1", len(decisions))
	}
	if got := decisions[0].Body.Payload["proposed_fact_id"]; got != factID {
		t.Errorf("decision proposed_fact_id = %v, want %s", got, factID)
	}
}

// TestRecover_MergeReviewDecision pins the decided pair across a restart.
func TestRecover_MergeReviewDecision(t *testing.T) {
	store, path := fileStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, err := mgr.Create("review2:parent", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child, wt := forkWorktreeChild(t, mgr, parent.ID(), "review2:child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("decide me\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	proposal, err := mgr.ProposeMerge(ctx, child.ID(), 100)
	if err != nil {
		t.Fatalf("ProposeMerge: %v", err)
	}
	factID, err := mgr.RecordMergeProposal(ctx, proposal)
	if err != nil {
		t.Fatalf("RecordMergeProposal: %v", err)
	}
	if _, err := mgr.RecordMergeDecision(ctx, proposal, false, "not yet", "discarded", factID); err != nil {
		t.Fatalf("RecordMergeDecision: %v", err)
	}

	recovered := recoverForkScopes(t, store, path, repo, map[string]string{
		"scope:review2:child": wt,
	})
	review, ok := recovered.MergeReview(child.ID())
	if !ok {
		t.Fatal("review not recovered")
	}
	if review.Decision == nil {
		t.Fatal("recovered review lost its decision")
	}
	if review.Decision.Approved || review.Decision.Reason != "not yet" || review.Decision.Action != "discarded" {
		t.Errorf("recovered decision = %+v, want denied/discarded", review.Decision)
	}
}
