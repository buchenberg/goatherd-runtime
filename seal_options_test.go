package goatherd

// Seal options: the retained diff, caller snapshot/metadata, the inline/blob
// split, the diff cap, and a rebuild across a simulated restart. These are
// the laws behind the goatherd-runtime child "seal carries the execution
// snapshot, diff, and opaque metadata".

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	trace "github.com/buchenberg/goatherd-kernel"
	sandbox "github.com/buchenberg/goatherd-runtime/sandbox"
	"github.com/buchenberg/goatherd-runtime/sandbox/git"
)

// sealOptionsFixture builds a parent and one worktree child with a small
// file written, returning the manager, store, repo, child, and worktree.
func sealOptionsFixture(t *testing.T, ownerPrefix string) (*ScopeManager, *trace.SQLiteTraceStore, string, string, *Scope, string) {
	t.Helper()
	store, path := fileStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	parent, err := mgr.Create(ownerPrefix+":parent", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child, wt := forkWorktreeChild(t, mgr, parent.ID(), ownerPrefix+":child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("line one\nline two\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	return mgr, store, path, repo, child, wt
}

// recoverSealOptions closes and reopens the store, then recovers with the
// worktree resolver the fixture's child needs.
func recoverSealOptions(t *testing.T, store *trace.SQLiteTraceStore, path, repo, wtPath, parentOwner, childOwner string) *ScopeManager {
	t.Helper()
	reopened := reopenStore(t, store, path)
	recovered, err := RecoverScopes(reopened, RecoverOptions{
		Resolver: func(scopeID, backend, path string) (sandbox.Sandbox, error) {
			switch scopeID {
			case "scope:" + parentOwner:
				return git.NewLocalGitSandbox(repo), nil
			case "scope:" + childOwner:
				return git.NewWorktreeSandbox(repo, wtPath), nil
			}
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("RecoverScopes: %v", err)
	}
	return recovered
}

func TestSealOptions_InlineValuesAcrossRecovery(t *testing.T) {
	ctx := context.Background()
	mgr, store, path, repo, child, wt := sealOptionsFixture(t, "sealopt")
	snapshot := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	metadata := []byte(`{"proposal":"p1"}`)

	out, err := mgr.Seal(ctx, child.ID(), SealOptions{Snapshot: snapshot, Metadata: metadata})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if got := out.Snapshot(); !bytes.Equal(got, snapshot) {
		t.Errorf("live Snapshot = %q, want %q", got, snapshot)
	}
	if got := out.Metadata(); !bytes.Equal(got, metadata) {
		t.Errorf("live Metadata = %q, want %q", got, metadata)
	}
	if out.Diff() == "" || !strings.Contains(out.Diff(), "feature.txt") {
		t.Errorf("live Diff = %q, want the file's diff", out.Diff())
	}
	if out.DiffTruncated() {
		t.Error("small diff reported truncated")
	}
	if out.DiffOriginalSize() != len(out.Diff()) {
		t.Errorf("DiffOriginalSize = %d, want %d", out.DiffOriginalSize(), len(out.Diff()))
	}

	// The getter must return a copy: mutating it cannot change the frozen
	// output.
	gotSnapshot := out.Snapshot()
	gotSnapshot[0] = 'X'
	if bytes.Equal(out.Snapshot(), gotSnapshot) {
		t.Error("Snapshot returned an alias of the retained bytes")
	}

	// Small values inline: no digest reference in the sealed fact.
	sealed := findRecords(t, store, "sealopt:child", SchemaRunOutputSealed)
	if len(sealed) != 1 {
		t.Fatalf("sealed records: %d, want 1", len(sealed))
	}
	payload := sealed[0].Body.Payload
	if _, ok := payload["snapshot_digest"]; ok {
		t.Error("small snapshot was externalized to a blob")
	}
	if _, ok := payload["snapshot"]; !ok {
		t.Error("small snapshot is neither inline nor blob-referenced")
	}
	if truncated, _ := payload["diff_truncated"].(bool); truncated {
		t.Error("payload diff_truncated = true for a small diff")
	}

	recovered := recoverSealOptions(t, store, path, repo, wt, "sealopt:parent", "sealopt:child")
	got, ok := recovered.Output(out.ID())
	if !ok {
		t.Fatal("output not recovered")
	}
	if got.Snapshot() == nil || !bytes.Equal(got.Snapshot(), snapshot) {
		t.Errorf("recovered Snapshot = %q, want %q", got.Snapshot(), snapshot)
	}
	if got.Metadata() == nil || !bytes.Equal(got.Metadata(), metadata) {
		t.Errorf("recovered Metadata = %q, want %q", got.Metadata(), metadata)
	}
	if got.Diff() != out.Diff() {
		t.Errorf("recovered Diff = %q, want %q", got.Diff(), out.Diff())
	}
	if got.DiffOriginalSize() != out.DiffOriginalSize() {
		t.Errorf("recovered DiffOriginalSize = %d, want %d", got.DiffOriginalSize(), out.DiffOriginalSize())
	}
	if got.DiffTruncated() {
		t.Error("recovered output reports a truncated diff")
	}
}

func TestSealOptions_LargeValuesRideBlobs(t *testing.T) {
	ctx := context.Background()
	mgr, store, path, repo, child, wt := sealOptionsFixture(t, "sealblob")
	snapshot := bytes.Repeat([]byte("S"), sealInlineLimit+1)
	metadata := bytes.Repeat([]byte("M"), sealInlineLimit*2)

	out, err := mgr.Seal(ctx, child.ID(), SealOptions{Snapshot: snapshot, Metadata: metadata})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	sealed := findRecords(t, store, "sealblob:child", SchemaRunOutputSealed)
	if len(sealed) != 1 {
		t.Fatalf("sealed records: %d, want 1", len(sealed))
	}
	payload := sealed[0].Body.Payload
	if _, ok := payload["snapshot"]; ok {
		t.Error("large snapshot was inlined in the fact")
	}
	if digest, _ := payload["snapshot_digest"].(string); digest == "" {
		t.Fatal("large snapshot has no digest reference")
	}

	// The bytes live in the kernel's named-blob store under the output's
	// owner path and id.
	data, digest, err := trace.GetBlob(ctx, store, "sealblob:child", sealBlobSnapshot, out.ID())
	if err != nil {
		t.Fatalf("GetBlob snapshot: %v", err)
	}
	if !bytes.Equal(data, snapshot) {
		t.Error("stored snapshot bytes differ from what Seal retained")
	}
	if want := fmt.Sprint(payload["snapshot_digest"]); digest != want {
		t.Errorf("blob digest = %q, want the fact's %q", digest, want)
	}

	recovered := recoverSealOptions(t, store, path, repo, wt, "sealblob:parent", "sealblob:child")
	got, ok := recovered.Output(out.ID())
	if !ok {
		t.Fatal("output not recovered")
	}
	if !bytes.Equal(got.Snapshot(), snapshot) {
		t.Error("recovered snapshot differs from the sealed bytes")
	}
	if !bytes.Equal(got.Metadata(), metadata) {
		t.Error("recovered metadata differs from the sealed bytes")
	}
}

func TestSealOptions_DiffCapTruncatesWithMarker(t *testing.T) {
	ctx := context.Background()
	mgr, store, path, repo, child, wt := sealOptionsFixture(t, "sealcap")
	// Make the diff comfortably larger than the cap.
	var big strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&big, "line %03d: the quick brown fox jumps over the lazy dog\n", i)
	}
	if err := os.WriteFile(filepath.Join(wt, "big.txt"), []byte(big.String()), 0o644); err != nil {
		t.Fatalf("write big file: %v", err)
	}

	const cap = 400
	out, err := mgr.Seal(ctx, child.ID(), SealOptions{DiffCap: cap})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !out.DiffTruncated() {
		t.Fatal("large diff was not truncated")
	}
	if out.DiffOriginalSize() <= cap {
		t.Errorf("DiffOriginalSize = %d, want over the %d-byte cap", out.DiffOriginalSize(), cap)
	}
	if !strings.Contains(out.Diff(), "diff truncated") {
		t.Errorf("truncated diff has no marker: %q", out.Diff())
	}

	sealed := findRecords(t, store, "sealcap:child", SchemaRunOutputSealed)
	if len(sealed) != 1 {
		t.Fatalf("sealed records: %d, want 1", len(sealed))
	}
	if truncated, _ := sealed[0].Body.Payload["diff_truncated"].(bool); !truncated {
		t.Error("payload does not record diff_truncated")
	}

	recovered := recoverSealOptions(t, store, path, repo, wt, "sealcap:parent", "sealcap:child")
	got, ok := recovered.Output(out.ID())
	if !ok {
		t.Fatal("output not recovered")
	}
	if !got.DiffTruncated() {
		t.Error("recovered output lost diff_truncated")
	}
	if got.DiffOriginalSize() != out.DiffOriginalSize() {
		t.Errorf("recovered DiffOriginalSize = %d, want %d", got.DiffOriginalSize(), out.DiffOriginalSize())
	}
	if got.Diff() != out.Diff() {
		t.Errorf("recovered Diff = %q, want %q", got.Diff(), out.Diff())
	}
}

func TestBoundDiff_NeverExceedsCap(t *testing.T) {
	diff := strings.Repeat("line: the quick brown fox\n", 100)
	for _, cap := range []int{1, 2, 5, 17, 64, 128, 256, 512} {
		got, truncated := boundDiff(diff, cap)
		if !truncated {
			t.Fatalf("cap %d: large diff reported untruncated", cap)
		}
		if len(got) > cap {
			t.Errorf("cap %d: retained %d bytes, over the cap", cap, len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("cap %d: retained text is not valid UTF-8", cap)
		}
	}
	if got, truncated := boundDiff(diff, len(diff)); truncated || got != diff {
		t.Error("diff exactly at the cap was not returned verbatim")
	}
	if got, truncated := boundDiff("small diff", 0); truncated || got != "small diff" {
		t.Error("small diff under the default cap was not returned verbatim")
	}
}

func TestSeal_DefaultPersistsDiffAcrossRecovery(t *testing.T) {
	ctx := context.Background()
	mgr, store, path, repo, child, wt := sealOptionsFixture(t, "sealdefault")
	out, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if out.Diff() == "" {
		t.Fatal("default seal retained no diff")
	}
	if out.Snapshot() != nil || out.Metadata() != nil {
		t.Error("default seal invented snapshot/metadata")
	}

	recovered := recoverSealOptions(t, store, path, repo, wt, "sealdefault:parent", "sealdefault:child")
	got, ok := recovered.Output(out.ID())
	if !ok {
		t.Fatal("output not recovered")
	}
	if got.Diff() != out.Diff() {
		t.Errorf("recovered Diff = %q, want %q", got.Diff(), out.Diff())
	}
}

func TestSeal_RejectsMultipleOptionSets(t *testing.T) {
	ctx := context.Background()
	mgr, _, _, _, child, _ := sealOptionsFixture(t, "sealmulti")
	if _, err := mgr.Seal(ctx, child.ID(), SealOptions{}, SealOptions{}); err == nil {
		t.Fatal("Seal accepted two option sets")
	}
}

func TestRecover_SealedBlobMissingLeavesValueAbsent(t *testing.T) {
	ctx := context.Background()
	mgr, store, path, repo, child, wt := sealOptionsFixture(t, "sealloss")
	snapshot := bytes.Repeat([]byte("S"), sealInlineLimit+1)
	out, err := mgr.Seal(ctx, child.ID(), SealOptions{Snapshot: snapshot})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := trace.DeleteBlob(ctx, store, "sealloss:child", sealBlobSnapshot, out.ID()); err != nil {
		t.Fatalf("DeleteBlob: %v", err)
	}

	recovered := recoverSealOptions(t, store, path, repo, wt, "sealloss:parent", "sealloss:child")
	got, ok := recovered.Output(out.ID())
	if !ok {
		t.Fatal("output not recovered")
	}
	if got.Snapshot() != nil {
		t.Errorf("recovered Snapshot = %q, want nil for a missing blob", got.Snapshot())
	}
	if got.Diff() == "" {
		t.Error("a missing snapshot blob also lost the diff")
	}
}
