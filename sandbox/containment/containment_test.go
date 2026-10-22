package containment

import (
	"errors"

	"github.com/buchenberg/goatherd-runtime/sandbox/confinement"
	"os"
	"path/filepath"
	"testing"
)

// fakeBackend is a containment.Backend double for contract tests.
type fakeBackend struct {
	available bool
	probeErr  error
	probed    bool
	launched  bool
	result    LaunchResult
}

func (f *fakeBackend) Name() string            { return "fake" }
func (f *fakeBackend) EnforcementTier() string { return "test" }
func (f *fakeBackend) Available() (bool, string) {
	if f.available {
		return true, "ok"
	}
	return false, "unavailable"
}
func (f *fakeBackend) ProfileFor([]string, confinement.NetworkPolicy) string { return "[]" }
func (f *fakeBackend) SupportsNetwork(confinement.NetMode) bool              { return true }
func (f *fakeBackend) Probe(string, string, []string) error {
	f.probed = true
	return f.probeErr
}
func (f *fakeBackend) Launch(string, LaunchRequest) (LaunchResult, error) {
	f.launched = true
	return f.result, nil
}

func TestReadOnlyAndPermissiveSpecs(t *testing.T) {
	ro := confinement.ReadOnlySpec()
	if len(ro.WritableRoots) != 0 || ro.Network.Mode != confinement.NetDenyAll {
		t.Fatalf("confinement.ReadOnlySpec = %+v", ro)
	}
	p := confinement.PermissiveFor("/x")
	if len(p.WritableRoots) != 1 || p.Network.Mode != confinement.NetAllowAll {
		t.Fatalf("confinement.PermissiveFor = %+v", p)
	}
}

func TestResolveSpec_Broker(t *testing.T) {
	// A broker policy without a port cannot be pinned — refuse.
	if _, _, err := confinement.ResolveSpec(confinement.ConfinementSpec{Network: confinement.NetworkPolicy{Mode: confinement.NetBroker}}); !errors.Is(err, confinement.ErrUnsupportedConfinementSpec) {
		t.Fatalf("broker without a port: want confinement.ErrUnsupportedConfinementSpec, got %v", err)
	}
	// With a port it resolves; the backend gates enforceability (SupportsNetwork).
	_, network, err := confinement.ResolveSpec(confinement.ConfinementSpec{Network: confinement.NetworkPolicy{Mode: confinement.NetBroker, BrokerPort: 8080}})
	if err != nil || network.Mode != confinement.NetBroker || network.BrokerPort != 8080 {
		t.Fatalf("broker with a port: %+v, %v", network, err)
	}
}

func TestResolveSpec_CanonicalizesRoots(t *testing.T) {
	root := t.TempDir()
	unclean := filepath.Join(root, "a", "..")
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	roots, network, err := confinement.ResolveSpec(confinement.ConfinementSpec{
		WritableRoots: []string{unclean},
		Network:       confinement.NetworkPolicy{Mode: confinement.NetAllowAll},
	})
	if err != nil {
		t.Fatal(err)
	}
	if network.Mode != confinement.NetAllowAll {
		t.Fatalf("network mode = %q, want allow_all", network.Mode)
	}
	if len(roots) != 1 || roots[0] != confinement.Canonical(root) {
		t.Fatalf("roots = %v, want [%s]", roots, confinement.Canonical(root))
	}
}

func TestLaunchConfined_ProbesFirst(t *testing.T) {
	fb := &fakeBackend{available: true, result: LaunchResult{ExitCode: 0}}
	_, err := LaunchConfined(fb, confinement.PermissiveFor(t.TempDir()), LaunchRequest{WorkingRoot: t.TempDir(), Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if !fb.probed || !fb.launched {
		t.Fatalf("probe must run before launch (probed=%v launched=%v)", fb.probed, fb.launched)
	}
}

func TestLaunchConfined_RefusesWhenUnavailable(t *testing.T) {
	fb := &fakeBackend{available: false}
	_, err := LaunchConfined(fb, confinement.ReadOnlySpec(), LaunchRequest{WorkingRoot: t.TempDir(), Command: []string{"true"}})
	if !errors.Is(err, ErrPaddockNotEstablished) {
		t.Fatalf("want ErrPaddockNotEstablished, got %v", err)
	}
	if fb.probed {
		t.Fatal("must not probe when the backend is unavailable")
	}
}

func TestLaunchConfined_ProbeFailureRefuses(t *testing.T) {
	fb := &fakeBackend{available: true, probeErr: ErrPaddockNotEstablished}
	_, err := LaunchConfined(fb, confinement.ReadOnlySpec(), LaunchRequest{WorkingRoot: t.TempDir(), Command: []string{"true"}})
	if !errors.Is(err, ErrPaddockNotEstablished) {
		t.Fatalf("want ErrPaddockNotEstablished, got %v", err)
	}
	if fb.launched {
		t.Fatal("must not launch when the probe refuses")
	}
}

func TestLaunchConfined_NilBackend(t *testing.T) {
	_, err := LaunchConfined(nil, confinement.ReadOnlySpec(), LaunchRequest{Command: []string{"true"}})
	if !errors.Is(err, ErrPaddockNotEstablished) {
		t.Fatalf("want ErrPaddockNotEstablished, got %v", err)
	}
}

func TestCapability_RefusesWithoutBackend(t *testing.T) {
	cap := Capability{WorkingRoot: t.TempDir(), Isolation: "ground"}
	if _, err := cap.LaunchConfined(confinement.ReadOnlySpec(), LaunchRequest{Command: []string{"true"}}); !errors.Is(err, ErrPaddockNotEstablished) {
		t.Fatalf("want ErrPaddockNotEstablished, got %v", err)
	}
}

func TestNegotiation_RefusesWithoutCapability(t *testing.T) {
	if err := RequireExecutionAuthority(nil); !errors.Is(err, ErrExecutionAuthorityRequired) {
		t.Fatalf("nil capability: want ErrExecutionAuthorityRequired, got %v", err)
	}
	if err := RequireExecutionAuthority(&Capability{}); !errors.Is(err, ErrExecutionAuthorityRequired) {
		t.Fatalf("backend-less capability: want ErrExecutionAuthorityRequired, got %v", err)
	}
	if err := RequireExecutionAuthority(&Capability{Backend: &fakeBackend{available: true}}); err != nil {
		t.Fatalf("a bound capability must pass the guard, got %v", err)
	}
}

func TestDeniedExit_ClassifiesSyscallDenial(t *testing.T) {
	cases := []struct {
		name string
		res  LaunchResult
		want bool
	}{
		{"clean exit", LaunchResult{ExitCode: 0}, false},
		{"permission denied", LaunchResult{ExitCode: 1, Stderr: "touch: cannot touch '/x': Permission denied"}, true},
		{"operation not permitted", LaunchResult{ExitCode: 1, Stderr: "ln: Operation not permitted"}, true},
		// Windows phrases the same denial differently, and without this the
		// Windows backend's denials are recorded as ordinary failures.
		{"access is denied", LaunchResult{ExitCode: 1, Stderr: "open C:\\x: Access is denied."}, true},
		{"unrelated failure", LaunchResult{ExitCode: 1, Stderr: "boom"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeniedExit(tc.res); got != tc.want {
				t.Fatalf("DeniedExit = %v, want %v", got, tc.want)
			}
			err := DenyError(tc.res, "ctx")
			if tc.want && !errors.Is(err, ErrSandboxDenied) {
				t.Fatalf("DenyError = %v, want ErrSandboxDenied", err)
			}
			if !tc.want && err != nil {
				t.Fatalf("DenyError = %v, want nil", err)
			}
		})
	}
}
