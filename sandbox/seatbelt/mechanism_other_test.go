//go:build !darwin

package seatbelt

import (
	"errors"
	"strings"
	"testing"

	"github.com/buchenberg/goatherd-runtime/sandbox/confinement"
	"github.com/buchenberg/goatherd-runtime/sandbox/containment"
)

func TestAvailableOffDarwin(t *testing.T) {
	ok, why := New().Available()
	if ok {
		t.Fatal("the Seatbelt backend reported itself available off macOS")
	}
	if !strings.Contains(why, "macOS") {
		t.Fatalf("unavailable reason %q does not name macOS", why)
	}
}

func TestDetectOffDarwin(t *testing.T) {
	if b := Detect(); b != nil {
		t.Fatalf("Detect = %v off macOS, want nil", b)
	}
}

func TestProbeRefusesOffDarwin(t *testing.T) {
	err := New().Probe(New().ProfileFor(nil, confinement.NetworkPolicy{Mode: confinement.NetDenyAll}), t.TempDir(), nil)
	if !errors.Is(err, containment.ErrPaddockNotEstablished) {
		t.Fatalf("Probe = %v, want ErrPaddockNotEstablished", err)
	}
}

func TestLaunchRefusesOffDarwin(t *testing.T) {
	_, err := New().Launch(New().ProfileFor(nil, confinement.NetworkPolicy{Mode: confinement.NetDenyAll}), containment.LaunchRequest{
		Command:     []string{"/bin/true"},
		WorkingRoot: t.TempDir(),
	})
	if !errors.Is(err, containment.ErrPaddockNotEstablished) {
		t.Fatalf("Launch = %v, want ErrPaddockNotEstablished", err)
	}
}
