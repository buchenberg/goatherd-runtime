package winacl

import (
	"strings"
	"testing"

	"github.com/buchenberg/goatherd-runtime/sandbox/confinement"
	"github.com/buchenberg/goatherd-runtime/sandbox/containment"
)

func TestWriteSidForIsDeterministic(t *testing.T) {
	const root = `C:\work\repo`
	first := WriteSidFor(root)
	second := WriteSidFor(root)
	if first != second {
		t.Fatalf("the write identity is not deterministic: %q vs %q", first, second)
	}
	if !strings.HasPrefix(first, "S-1-4-") {
		t.Fatalf("the write identity %q is not in the S-1-4 form", first)
	}
	if parts := strings.Split(first, "-"); len(parts) != 5 {
		t.Fatalf("the write identity %q does not carry two subauthorities", first)
	}
}

func TestWriteSidForDistinguishesRoots(t *testing.T) {
	if WriteSidFor(`C:\work\a`) == WriteSidFor(`C:\work\b`) {
		t.Fatal("two roots derived the same write identity")
	}
}

func TestProfileForReadOnly(t *testing.T) {
	b := New()
	doc := b.ProfileFor(nil, confinement.NetworkPolicy{Mode: confinement.NetDenyAll})
	prof, err := decodeProfile(doc)
	if err != nil {
		t.Fatalf("decode the profile: %v", err)
	}
	if prof.Mode != modeReadOnly {
		t.Fatalf("a profile with no roots is %q, want %q", prof.Mode, modeReadOnly)
	}
	if len(prof.SIDs) != 0 {
		t.Fatalf("a read-only profile carries %d write SIDs", len(prof.SIDs))
	}
	if err := prof.validate(); err != nil {
		t.Fatalf("the read-only profile does not validate: %v", err)
	}
}

func TestProfileForWorkspaceWrite(t *testing.T) {
	b := New()
	roots := []string{`C:\work\a`, `C:\work\b`}
	doc := b.ProfileFor(roots, confinement.NetworkPolicy{Mode: confinement.NetAllowAll})
	prof, err := decodeProfile(doc)
	if err != nil {
		t.Fatalf("decode the profile: %v", err)
	}
	if prof.Mode != modeWorkspaceWrite {
		t.Fatalf("a profile with roots is %q, want %q", prof.Mode, modeWorkspaceWrite)
	}
	if len(prof.SIDs) != len(roots) {
		t.Fatalf("the profile carries %d write SIDs for %d roots", len(prof.SIDs), len(roots))
	}
	for i, root := range prof.Roots {
		if prof.SIDs[i] != WriteSidFor(root) {
			t.Fatalf("root %d has the write identity %q, want %q", i, prof.SIDs[i], WriteSidFor(root))
		}
	}
	if err := prof.validate(); err != nil {
		t.Fatalf("the workspace-write profile does not validate: %v", err)
	}
}

func TestProfileValidateRefusesAMismatchedIdentity(t *testing.T) {
	prof := profile{
		Roots: []string{`C:\work\a`},
		SIDs:  []string{WriteSidFor(`C:\work\b`)},
		Mode:  modeWorkspaceWrite,
	}
	if err := prof.validate(); err == nil {
		t.Fatal("a profile whose SID does not match its root was accepted")
	}
}

func TestProfileValidateRefusesAnUnknownMode(t *testing.T) {
	if err := (profile{Mode: "permissive"}).validate(); err == nil {
		t.Fatal("a profile with an unknown mode was accepted")
	}
}

func TestSupportsNetworkOnlyAllowsAllowAll(t *testing.T) {
	b := New()
	if !b.SupportsNetwork(confinement.NetAllowAll) {
		t.Fatal("allow_all must be representable")
	}
	for _, mode := range []confinement.NetMode{confinement.NetDenyAll, confinement.NetBroker, ""} {
		if b.SupportsNetwork(mode) {
			t.Fatalf("the mechanism has no egress control, so %q must not be representable", mode)
		}
	}
}

func TestEnforcementTierIsNotSyscallDeny(t *testing.T) {
	// The tier names the mechanism's shape. Windows enforces in the access
	// check, not in a syscall filter, and the difference is load-bearing for
	// anyone reasoning about what the boundary covers.
	if got := New().EnforcementTier(); got != "native-access-check-deny" {
		t.Fatalf("enforcement tier is %q", got)
	}
}

func TestBackendImplementsTheContract(t *testing.T) {
	var _ containment.Backend = New()
}
