package seatbelt

import (
	"testing"

	"github.com/buchenberg/goatherd-runtime/sandbox/confinement"
	"github.com/buchenberg/goatherd-runtime/sandbox/containment"
)

func TestBackendImplementsTheContract(t *testing.T) {
	var _ containment.Backend = New()
}

func TestName(t *testing.T) {
	if got := New().Name(); got != "seatbelt" {
		t.Fatalf("name = %q, want %q", got, "seatbelt")
	}
}

func TestEnforcementTierIsDistinct(t *testing.T) {
	// Seatbelt is a per-process policy, not a syscall filter (Landlock) and not
	// an access check (winacl). A trace that claimed either would misreport the
	// mechanism the host used.
	if got := New().EnforcementTier(); got != "native-sandbox-deny" {
		t.Fatalf("enforcement tier = %q, want %q", got, "native-sandbox-deny")
	}
}

func TestSupportsNetwork(t *testing.T) {
	b := New()
	for _, mode := range []confinement.NetMode{
		confinement.NetAllowAll,
		confinement.NetDenyAll,
		confinement.NetBroker,
		"",
	} {
		if !b.SupportsNetwork(mode) {
			t.Fatalf("mode %q must be representable", mode)
		}
	}
	if b.SupportsNetwork("not-a-mode") {
		t.Fatal("an unknown network mode was reported as representable")
	}
}
