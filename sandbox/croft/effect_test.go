//go:build linux

package croft

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	trace "github.com/buchenberg/goatherd-kernel"
	"github.com/buchenberg/goatherd-runtime/effect"
	sbx "github.com/buchenberg/goatherd-runtime/sandbox"

	croftlib "github.com/buchenberg/goatherd-croft"
)

// The adapter must be discoverable as an EffectChanger by type assertion, which
// is how a host finds the capability without importing croft.
var _ EffectChanger = (*Sandbox)(nil)

func TestSandbox_EffectChanges_MapsEveryKind(t *testing.T) {
	s, fake := newTestSandbox(t)
	ctx := context.Background()
	if err := s.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ws, err := s.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	fake.mu.Lock()
	fake.changes = []croftlib.Change{
		{Path: "new.txt", Kind: croftlib.ChangeAdd, Mode: 0o644, Size: 5},
		{Path: "old.txt", Kind: croftlib.ChangeModify, Mode: 0o600, Size: 9},
		{Path: "gone.txt", Kind: croftlib.ChangeDelete},
	}
	fake.mu.Unlock()

	got, err := s.EffectChanges(ctx, ws)
	if err != nil {
		t.Fatalf("EffectChanges: %v", err)
	}
	want := []struct {
		path string
		kind effect.Kind
	}{
		{"new.txt", effect.KindCreated},
		{"old.txt", effect.KindPatched},
		{"gone.txt", effect.KindDeleted},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d changes, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Path != w.path || got[i].Kind != w.kind {
			t.Errorf("change %d = (%q, %q), want (%q, %q)", i, got[i].Path, got[i].Kind, w.path, w.kind)
		}
	}
	// croft hands back changes sorted by Path and a caller correlates them
	// against a diff, so the order must survive the translation.
	if got[0].Path != "new.txt" || got[2].Path != "gone.txt" {
		t.Errorf("ordering not preserved: %q … %q", got[0].Path, got[2].Path)
	}
}

func TestSandbox_EffectChanges_CarriesMetadata(t *testing.T) {
	s, fake := newTestSandbox(t)
	ctx := context.Background()
	if err := s.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ws, err := s.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	fake.mu.Lock()
	fake.changes = []croftlib.Change{
		{Path: "bin/tool", Kind: croftlib.ChangeModify, Mode: fs.ModeSetuid | 0o755, Size: 4096},
	}
	fake.mu.Unlock()

	got, err := s.EffectChanges(ctx, ws)
	if err != nil {
		t.Fatalf("EffectChanges: %v", err)
	}
	if got[0].Mode != fs.ModeSetuid|0o755 {
		t.Errorf("Mode = %v, want the setuid executable bit preserved", got[0].Mode)
	}
	if got[0].Size != 4096 {
		t.Errorf("Size = %d, want 4096", got[0].Size)
	}
	// effect's payload renders modes as Unix octal, so the setuid bit survives
	// into the record even though a host-side write would not propagate it.
	if m := effect.UnixOctal(got[0].Mode); m != "4755" {
		t.Errorf("UnixOctal = %q, want 4755", m)
	}
}

func TestSandbox_EffectChanges_DeletedSymlinkKeepsItsTypeBit(t *testing.T) {
	// croft distinguishes a deleted symlink from a deleted regular file by
	// setting the ModeSymlink type bit while leaving the permission bits zero.
	// The translation must not flatten that, or the distinction is gone before
	// any consumer could use it.
	s, fake := newTestSandbox(t)
	ctx := context.Background()
	if err := s.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ws, err := s.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	fake.mu.Lock()
	fake.changes = []croftlib.Change{
		{Path: "link", Kind: croftlib.ChangeDelete, Mode: fs.ModeSymlink | 0o777},
		{Path: "file", Kind: croftlib.ChangeDelete},
	}
	fake.mu.Unlock()

	got, err := s.EffectChanges(ctx, ws)
	if err != nil {
		t.Fatalf("EffectChanges: %v", err)
	}
	if got[0].Mode&fs.ModeSymlink == 0 {
		t.Errorf("the deleted symlink lost its ModeSymlink bit: mode = %v", got[0].Mode)
	}
	if got[1].Mode&fs.ModeSymlink != 0 {
		t.Errorf("the deleted regular file gained a symlink bit: mode = %v", got[1].Mode)
	}
}

func TestSandbox_EffectChanges_DirectoriesPassThrough(t *testing.T) {
	// The translation is faithful and effect.FromChanges decides policy. A
	// converter that pre-filtered directories would make it impossible for a
	// future caller to ask for directory effects without changing this seam.
	s, fake := newTestSandbox(t)
	ctx := context.Background()
	if err := s.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ws, err := s.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	fake.mu.Lock()
	fake.changes = []croftlib.Change{
		{Path: "pkg", Kind: croftlib.ChangeAdd, IsDir: true, Mode: fs.ModeDir | 0o755},
		{Path: "pkg/a.go", Kind: croftlib.ChangeAdd, Mode: 0o644, Size: 10},
	}
	fake.mu.Unlock()

	got, err := s.EffectChanges(ctx, ws)
	if err != nil {
		t.Fatalf("EffectChanges: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d changes, want 2 — the converter must not filter directories", len(got))
	}
	if !got[0].IsDir {
		t.Error("the directory entry lost its IsDir flag")
	}

	// And the effect layer is what drops it, yielding one file effect.
	effects, err := effect.FromChanges(got)
	if err != nil {
		t.Fatalf("FromChanges: %v", err)
	}
	if len(effects) != 1 || effects[0].Path != "pkg/a.go" {
		t.Errorf("FromChanges yielded %+v, want a single effect for pkg/a.go", effects)
	}
	if effects[0].SchemaRef != effect.SchemaFileCreated {
		t.Errorf("SchemaRef = %q, want %q", effects[0].SchemaRef, effect.SchemaFileCreated)
	}
}

func TestSandbox_EffectChanges_UnknownKindIsAnError(t *testing.T) {
	// If croft grows a classification this adapter has not been taught about,
	// the change must fail loudly. Silently dropping it would leave a workspace
	// mutation unrecorded with nothing saying so.
	s, fake := newTestSandbox(t)
	ctx := context.Background()
	if err := s.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ws, err := s.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	fake.mu.Lock()
	fake.changes = []croftlib.Change{
		{Path: "ok.txt", Kind: croftlib.ChangeAdd},
		{Path: "moved.txt", Kind: croftlib.ChangeKind(99)},
	}
	fake.mu.Unlock()

	_, err = s.EffectChanges(ctx, ws)
	if err == nil {
		t.Fatal("an unmapped change kind must be an error, not a skipped entry")
	}
	if !strings.Contains(err.Error(), "moved.txt") {
		t.Errorf("the error must name the offending path, got: %v", err)
	}
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("the error must name the unmapped kind value, got: %v", err)
	}
}

func TestSandbox_EffectChanges_EmptyAndForeignState(t *testing.T) {
	s, fake := newTestSandbox(t)
	ctx := context.Background()
	if err := s.Create(ctx, sbx.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ws, err := s.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	fake.mu.Lock()
	fake.changes = []croftlib.Change{}
	fake.mu.Unlock()

	got, err := s.EffectChanges(ctx, ws)
	if err != nil {
		t.Fatalf("EffectChanges on a clean workspace: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d changes, want 0", len(got))
	}

	// A state this backend did not produce is rejected by the same provenance
	// check the rest of the adapter uses, before any daemon call.
	if _, err := s.EffectChanges(ctx, trace.WorkspaceState{Backend: "git", Revision: "abc"}); err == nil {
		t.Error("EffectChanges must reject a state from another backend")
	}
}

func TestSandbox_EffectChanges_DetectedByTypeAssertion(t *testing.T) {
	// The path a host actually takes: hold the runtime's Sandbox interface,
	// assert the optional capability, consume neutral types. No croft import
	// anywhere in this flow — which is the point of the translation.
	s, _ := newTestSandbox(t)
	var sb any = s

	changer, ok := sb.(interface {
		EffectChanges(context.Context, trace.WorkspaceState) ([]effect.Change, error)
	})
	if !ok {
		t.Fatal("the adapter must be discoverable as an EffectChanger by structural type assertion")
	}
	if _, isNamed := sb.(EffectChanger); !isNamed {
		t.Error("the adapter does not satisfy the declared EffectChanger interface")
	}
	_ = changer
}
