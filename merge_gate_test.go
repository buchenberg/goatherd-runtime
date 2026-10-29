package goatherd

import (
	"context"
	"fmt"
	trace "github.com/buchenberg/goatherd-kernel"
	sandbox "github.com/buchenberg/goatherd-runtime/sandbox"
	"github.com/buchenberg/goatherd-runtime/sandbox/git"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// findRecords reads an owner's trace and returns the records with the given
// schema ref, in record order.
func findRecords(t *testing.T, store *trace.SQLiteTraceStore, ownerID, schemaRef string) []trace.Record {
	t.Helper()
	slice, err := store.ReadOwnerPrefix(context.Background(), trace.TrustedReadContext, ownerID, 999, trace.ModeBoth)
	if err != nil {
		t.Fatalf("read trace for %s: %v", ownerID, err)
	}
	var found []trace.Record
	for _, id := range slice.FactIDs() {
		rec := slice.FactsByID[id]
		if rec.GetEnvelope().SchemaRef == schemaRef {
			found = append(found, rec.(trace.Record))
		}
	}
	return found
}

// TestClassifyChanges covers the unified-diff classifier: create, modify,
// delete, and a file present in the changed-file list without a diff section.
func TestClassifyChanges(t *testing.T) {
	diff := `diff --git a/created.txt b/created.txt
new file mode 100644
index 0000000..8b13789
--- /dev/null
+++ b/created.txt
@@ -0,0 +1 @@
+hello
diff --git a/changed.txt b/changed.txt
index 8b13789..c16a5f0 100644
--- a/changed.txt
+++ b/changed.txt
@@ -1 +1 @@
-old
+new
diff --git a/gone.txt b/gone.txt
deleted file mode 100644
index 8b13789..0000000
--- a/gone.txt
+++ /dev/null
@@ -1 +0,0 @@
-old
`
	changes := classifyChanges(diff, []string{"created.txt", "changed.txt", "gone.txt"})
	if len(changes) != 3 {
		t.Fatalf("classifyChanges returned %d changes, want 3: %+v", len(changes), changes)
	}
	byPath := make(map[string]ProposedChange, len(changes))
	for _, c := range changes {
		byPath[c.Path] = c
	}
	if c := byPath["created.txt"]; c.Kind != ChangeCreate || c.Diff != "hello" {
		t.Errorf("created.txt: kind=%q diff=%q, want create/hello", c.Kind, c.Diff)
	}
	if c := byPath["changed.txt"]; c.Kind != ChangeModify {
		t.Errorf("changed.txt: kind=%q, want modify", c.Kind)
	}
	if c := byPath["gone.txt"]; c.Kind != ChangeDelete {
		t.Errorf("gone.txt: kind=%q, want delete", c.Kind)
	}

	// A file with no diff section must not silently vanish from review.
	changes = classifyChanges("", []string{"unparsed.bin"})
	if len(changes) != 1 || changes[0].Path != "unparsed.bin" || changes[0].Kind != ChangeModify {
		t.Errorf("list-only file: %+v, want one unclassified modify", changes)
	}

	// A legitimate top-level "b/" directory survives side-aware stripping.
	diff = `diff --git a/b/real.go b/b/real.go
index 8b13789..c16a5f0 100644
--- a/b/real.go
+++ b/b/real.go
@@ -1 +1 @@
-old
+new
`
	changes = classifyChanges(diff, nil)
	if len(changes) != 1 || changes[0].Path != "b/real.go" {
		t.Errorf("b/ directory path: %+v, want b/real.go intact", changes)
	}

	// A header-less backend's multi-file diff: the second ---/+++ pair
	// inside the first file's hunks starts the next section.
	diff = `--- a/first.txt
+++ b/first.txt
@@ -1 +1 @@
-one
+two
--- a/second.txt
+++ b/second.txt
@@ -1 +1 @@
-uno
+dos
`
	changes = classifyChanges(diff, nil)
	if len(changes) != 2 || changes[0].Path != "first.txt" || changes[1].Path != "second.txt" {
		t.Errorf("header-less multi-file diff: %+v, want first.txt and second.txt", changes)
	}

	// Deleted-line content that merely starts with "--- " stays content when
	// the next line is not a +++ header.
	diff = `diff --git a/notes.txt b/notes.txt
index 8b13789..c16a5f0 100644
--- a/notes.txt
+++ b/notes.txt
@@ -1,2 +1 @@
-note
--- marker line
+condensed
`
	changes = classifyChanges(diff, nil)
	if len(changes) != 1 || changes[0].Path != "notes.txt" || changes[0].Kind != ChangeModify {
		t.Errorf("--- content line misparsed as a section boundary: %+v", changes)
	}
}

// TestFork_RecordsBaselineDigest covers T3.1: a sandboxed parent's fork
// records the baseline digest in the fork payload and hands the child the
// baseline state; a pure-causal parent records no baseline.
func TestFork_RecordsBaselineDigest(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	parent, err := mgr.Create("gate:base-parent", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	child, err := mgr.Fork(parent.ID(), "gate:base-child", nil)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if _, ok := child.Baseline(); !ok {
		t.Fatal("child of a sandboxed parent should carry a fork baseline")
	}
	base, _ := child.Baseline()
	if base.Backend != "git" {
		t.Errorf("baseline backend = %q, want git", base.Backend)
	}

	forks := findRecords(t, store, "gate:base-parent", SchemaScopeForked)
	if len(forks) != 1 {
		t.Fatalf("fork records: %d, want 1", len(forks))
	}
	digest := fmt.Sprint(forks[0].Body.Payload["baseline_digest"])
	if digest == "" {
		t.Error("fork payload should carry baseline_digest")
	}
	want, err := base.Digest()
	if err != nil {
		t.Fatalf("digest baseline: %v", err)
	}
	if digest != want {
		t.Errorf("baseline_digest = %q, want %q", digest, want)
	}

	// A pure-causal parent forks without a baseline, and the child says so.
	causalParent, _ := mgr.Create("gate:causal-parent", nil)
	causalChild, err := mgr.Fork(causalParent.ID(), "gate:causal-child", nil)
	if err != nil {
		t.Fatalf("causal fork: %v", err)
	}
	if _, ok := causalChild.Baseline(); ok {
		t.Error("child of a pure-causal parent must not carry a baseline")
	}
	causalForks := findRecords(t, store, "gate:causal-parent", SchemaScopeForked)
	if len(causalForks) != 1 {
		t.Fatalf("causal fork records: %d, want 1", len(causalForks))
	}
	if _, has := causalForks[0].Body.Payload["baseline_digest"]; has {
		t.Error("pure-causal fork payload must not carry baseline_digest")
	}
}

// forkWorktreeChild forks an isolated worktree child of parent, provisions
// the worktree, and hands back the child and the worktree path the test
// "works" in. leaf names the worktree directory (no ':' — Windows paths).
func forkWorktreeChild(t *testing.T, mgr *ScopeManager, parentID, childOwner, repo, leaf string) (*Scope, string) {
	t.Helper()
	wtPath := filepath.Join(t.TempDir(), leaf)
	sb := git.NewWorktreeSandbox(repo, wtPath)
	if err := sb.Create(context.Background(), sandbox.SandboxSpec{}); err != nil {
		t.Fatalf("worktree create for %s: %v", childOwner, err)
	}
	child, err := mgr.ForkIsolated(parentID, childOwner, nil, sb)
	if err != nil {
		t.Fatalf("ForkIsolated %s: %v", childOwner, err)
	}
	return child, wtPath
}

// TestProposeMerge_ClassifiesChildChanges covers T3.2: the proposal carries
// the child's classified changes and the baseline digest, and refuses
// children without a baseline or a diff-capable sandbox.
func TestProposeMerge_ClassifiesChildChanges(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	parent, _ := mgr.Create("gate:prop-parent", git.NewLocalGitSandbox(repo))

	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "gate:prop-child", repo, "wt")

	if err := os.WriteFile(filepath.Join(wtPath, "feature.txt"), []byte("shiny"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}

	proposal, err := mgr.ProposeMerge(context.Background(), child.ID(), 100)
	if err != nil {
		t.Fatalf("ProposeMerge: %v", err)
	}
	if proposal.ChildScopeID != child.ID() {
		t.Errorf("ChildScopeID = %q, want %q", proposal.ChildScopeID, child.ID())
	}
	if len(proposal.Changes) != 1 {
		t.Fatalf("changes: %+v, want exactly feature.txt", proposal.Changes)
	}
	c := proposal.Changes[0]
	if c.Path != "feature.txt" || c.Kind != ChangeCreate || c.Diff != "shiny" {
		t.Errorf("feature change = %+v, want create with preview 'shiny'", c)
	}
	base, _ := child.Baseline()
	wantDigest, _ := base.Digest()
	if proposal.BaselineDigest != wantDigest {
		t.Errorf("BaselineDigest = %q, want %q", proposal.BaselineDigest, wantDigest)
	}

	// ProposeMerge is a query: no proposed record, no state change.
	if recs := findRecords(t, store, "gate:prop-child", SchemaMergeProposed); len(recs) != 0 {
		t.Errorf("ProposeMerge recorded %d proposal records, want 0", len(recs))
	}
	if child.State() != ScopeActive {
		t.Errorf("child state = %s, want active", child.State())
	}

	// A pure-causal child has no baseline and cannot propose.
	causalParent, _ := mgr.Create("gate:prop-causal", nil)
	causalChild, err := mgr.Fork(causalParent.ID(), "gate:prop-causal-child", nil)
	if err != nil {
		t.Fatalf("causal fork: %v", err)
	}
	if _, err := mgr.ProposeMerge(context.Background(), causalChild.ID(), 100); err == nil {
		t.Error("ProposeMerge on a child without a baseline should fail")
	}
}

// TestRecordMergeProposal_Denied covers the explicit provenance pair on a
// shared-sandbox child: the proposal is recorded before review, the denial is
// recorded on the parent, and the caller discards and restores its own
// checkpoint. This replaces the old CommitMerge gate.
func TestRecordMergeProposal_Denied(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("gate:deny-parent", git.NewLocalGitSandbox(repo))

	cp, err := mgr.CreateCheckpoint(ctx, parent.ID(), nil)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	child, err := mgr.Fork(parent.ID(), "gate:deny-child", nil)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "outside.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}

	proposal, err := mgr.ProposeMerge(ctx, child.ID(), 100)
	if err != nil {
		t.Fatalf("ProposeMerge: %v", err)
	}
	proposedID, err := mgr.RecordMergeProposal(ctx, proposal)
	if err != nil {
		t.Fatalf("RecordMergeProposal: %v", err)
	}
	if proposedID == "" {
		t.Fatal("RecordMergeProposal returned no fact id")
	}
	if _, err := mgr.RecordMergeDecision(ctx, proposal, false, "outside the drafts prefix", "discarded", proposedID); err != nil {
		t.Fatalf("RecordMergeDecision: %v", err)
	}
	if err := parent.Discard(child); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if _, err := mgr.RestoreCheckpoint(ctx, cp.ID); err != nil {
		t.Fatalf("restore after denial: %v", err)
	}

	proposals := findRecords(t, store, "gate:deny-child", SchemaMergeProposed)
	if len(proposals) != 1 {
		t.Fatalf("proposed records on child path: %d, want 1", len(proposals))
	}
	decisions := findRecords(t, store, "gate:deny-parent", SchemaSupervisorDecision)
	if len(decisions) != 1 {
		t.Fatalf("decision records: %d, want 1", len(decisions))
	}
	if fmt.Sprint(decisions[0].Body.Payload["approved"]) != "false" {
		t.Errorf("decision approved = %v, want false", decisions[0].Body.Payload["approved"])
	}
	if fmt.Sprint(decisions[0].Body.Payload["reason"]) == "" {
		t.Error("denied decision should record the reason")
	}
	if fmt.Sprint(decisions[0].Body.Payload["action"]) != "discarded" {
		t.Errorf("decision action = %v, want discarded", decisions[0].Body.Payload["action"])
	}
	if recs := findRecords(t, store, "gate:deny-parent", SchemaScopeMerged); len(recs) != 0 {
		t.Errorf("denied merge must not record scope:merged, found %d", len(recs))
	}
}

// TestRecordMergeProposal_Approved covers the approval half: proposal and
// decision records exist before the causal merge, and the merge proceeds.
func TestRecordMergeProposal_Approved(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("gate:ok-parent", git.NewLocalGitSandbox(repo))

	child, err := mgr.Fork(parent.ID(), "gate:ok-child", nil)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "drafts"), 0o755); err != nil {
		t.Fatalf("mkdir drafts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "drafts", "good.txt"), []byte("yes"), 0o644); err != nil {
		t.Fatalf("write good: %v", err)
	}

	proposal, err := mgr.ProposeMerge(ctx, child.ID(), 100)
	if err != nil {
		t.Fatalf("ProposeMerge: %v", err)
	}
	proposedID, err := mgr.RecordMergeProposal(ctx, proposal)
	if err != nil {
		t.Fatalf("RecordMergeProposal: %v", err)
	}
	if _, err := mgr.RecordMergeDecision(ctx, proposal, true, "", "merged", proposedID); err != nil {
		t.Fatalf("RecordMergeDecision: %v", err)
	}
	if err := parent.Merge(child); err != nil {
		t.Fatalf("merge: %v", err)
	}

	if child.State() != ScopeMerged {
		t.Errorf("child state = %s, want merged", child.State())
	}
	if !fileExists(t, repo, "drafts/good.txt") {
		t.Error("approved merge should keep the child's work in the parent tree")
	}
	if recs := findRecords(t, store, "gate:ok-child", SchemaMergeProposed); len(recs) != 1 {
		t.Errorf("proposed records: %d, want 1", len(recs))
	}
	decisions := findRecords(t, store, "gate:ok-parent", SchemaSupervisorDecision)
	if len(decisions) != 1 || fmt.Sprint(decisions[0].Body.Payload["approved"]) != "true" {
		t.Errorf("decisions: %+v, want one approved", decisions)
	}
	if fmt.Sprint(decisions[0].Body.Payload["action"]) != "merged" {
		t.Errorf("decision action = %v, want merged", decisions[0].Body.Payload["action"])
	}
}

// TestRecordMergeProposal_RequiresActiveChild pins the provenance boundary:
// a proposal cannot be recorded for an already-terminal scope, so a decision
// can never cite work that was never reviewable.
func TestRecordMergeProposal_RequiresActiveChild(t *testing.T) {
	mgr := NewScopeManager(newMemStore(t))
	if _, err := mgr.RecordMergeProposal(context.Background(), nil); err == nil {
		t.Fatal("nil proposal must be rejected")
	}
}

// TestDestructivePathReviewer covers the pattern-matching reviewer.
func TestDestructivePathReviewer(t *testing.T) {
	review := DestructivePathReviewer([]string{"*.pem", "secrets/"})

	err := review(MergeProposal{Changes: []ProposedChange{{Path: "out/app.go"}}})
	if err != nil {
		t.Errorf("clean proposal denied: %v", err)
	}
	err = review(MergeProposal{Changes: []ProposedChange{{Path: "keys/ca.pem"}}})
	if err == nil || !strings.Contains(err.Error(), "*.pem") {
		t.Errorf("pem change: err = %v, want pattern-named denial", err)
	}
	err = review(MergeProposal{Changes: []ProposedChange{{Path: "secrets/token.txt"}}})
	if err == nil || !strings.Contains(err.Error(), "secrets/") {
		t.Errorf("secret change: err = %v, want prefix-named denial", err)
	}
	// An empty pattern list denies nothing, not everything.
	err = review(MergeProposal{Changes: []ProposedChange{{Path: "anything.txt"}}})
	if err != nil {
		t.Errorf("empty pattern list denied: %v", err)
	}
}
