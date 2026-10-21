//go:build linux

package landlock

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/buchenberg/shepherd-runtime-go/sandbox/containment"
)

// allowAll / denyAll are the network policies most tests use.
func allowAll() containment.NetworkPolicy {
	return containment.NetworkPolicy{Mode: containment.NetAllowAll}
}
func denyAll() containment.NetworkPolicy {
	return containment.NetworkPolicy{Mode: containment.NetDenyAll}
}

// TestMain installs the trampoline hook. Every confine-then-exec, confined
// write-probe, and confined net-probe re-execs this very test binary, so the hook
// must run before the test runner. It also dispatches the e2e connect helpers.
func TestMain(m *testing.M) {
	if code, handled := MaybeRunTrampoline(os.Args); handled {
		os.Exit(code)
	}
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case e2eConnectSentinel:
			if len(os.Args) < 4 {
				os.Exit(ConfineFailedRC) // a sentinel with missing operands must fail loudly, not run the suite
			}
			os.Exit(e2eConnect(os.Args[2], os.Args[3]))
		case e2eDirectSentinel:
			if len(os.Args) < 3 {
				os.Exit(ConfineFailedRC)
			}
			os.Exit(e2eDirect(os.Args[2]))
		}
	}
	os.Exit(m.Run())
}

func requireLandlock(t *testing.T) *Backend {
	t.Helper()
	b := New()
	if ok, why := b.Available(); !ok {
		t.Skipf("landlock unavailable: %s", why)
	}
	return b
}

func requireTouch(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/usr/bin/touch", "/bin/touch"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("no touch(1) on PATH")
	return ""
}

func TestLandlock_ABIDetection(t *testing.T) {
	if _, err := abi(); err != nil {
		if !errors.Is(err, containment.ErrUnsupportedKernel) {
			t.Fatalf("abi error must wrap ErrUnsupportedKernel, got %v", err)
		}
		t.Skipf("no landlock: %v", err)
	}
}

func TestLandlock_MaskIsV1Compatible(t *testing.T) {
	want := uint64((1 << 1) | (1 << 4) | (1 << 5) | (1 << 6) | (1 << 7) |
		(1 << 8) | (1 << 9) | (1 << 10) | (1 << 11) | (1 << 12))
	if uint64(WriteAccessMask) != want {
		t.Fatalf("WriteAccessMask = %#b, want %#b", WriteAccessMask, want)
	}
	for _, bit := range []uint{0, 2, 3, 13, 14, 15} {
		if WriteAccessMask&(1<<bit) != 0 {
			t.Fatalf("bit %d must NOT be governed (reads/exec/v2+/v3+/v5+ excluded)", bit)
		}
	}
}

func TestLandlock_ProfileFor(t *testing.T) {
	b := requireLandlock(t)
	wd := t.TempDir()

	doc := parseProfile(b.ProfileFor([]string{wd}, allowAll()))
	if len(doc.Roots) != 1 || doc.Roots[0] != containment.Canonical(wd) {
		t.Fatalf("profile roots = %v, want [%s]", doc.Roots, containment.Canonical(wd))
	}
	if doc.Net.Mode != "allow_all" {
		t.Fatalf("net mode = %q, want allow_all", doc.Net.Mode)
	}

	empty := parseProfile(b.ProfileFor(nil, denyAll()))
	if len(empty.Roots) != 0 || empty.Net.Mode != "deny_all" {
		t.Fatalf("empty profile = %+v", empty)
	}

	broker := parseProfile(b.ProfileFor([]string{wd}, containment.NetworkPolicy{Mode: containment.NetBroker, BrokerPort: 8080}))
	if broker.Net.Mode != "broker" || broker.Net.Port != 8080 {
		t.Fatalf("broker profile net = %+v", broker.Net)
	}
}

func TestLandlock_ConfineDeniesOutside(t *testing.T) {
	b := requireLandlock(t)
	touch := requireTouch(t)
	wd := t.TempDir()
	profile := b.ProfileFor([]string{wd}, allowAll())

	inside := filepath.Join(wd, "ok.txt")
	res, err := b.Launch(profile, containment.LaunchRequest{Command: []string{touch, inside}, WorkingRoot: wd})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("in-workdir write rc=%d stderr=%s", res.ExitCode, res.Stderr)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Fatalf("in-workdir file should exist: %v", err)
	}

	outside := filepath.Join(filepath.Dir(wd), "escape.txt")
	_ = os.Remove(outside)
	res, err = b.Launch(profile, containment.LaunchRequest{Command: []string{touch, outside}, WorkingRoot: wd})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 {
		t.Fatal("out-of-workdir write must be denied")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("out-of-workdir file must not exist")
	}
}

func TestLandlock_ReadOnlyDeniesEverywhere(t *testing.T) {
	b := requireLandlock(t)
	touch := requireTouch(t)
	wd := t.TempDir()
	profile := b.ProfileFor(nil, denyAll())

	target := filepath.Join(wd, "nope.txt")
	res, err := b.Launch(profile, containment.LaunchRequest{Command: []string{touch, target}, WorkingRoot: wd})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 {
		t.Fatal("readonly write must be denied")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("readonly file must not exist")
	}
}

func TestLandlock_PerBindingDenyClosed(t *testing.T) {
	b := requireLandlock(t)
	touch := requireTouch(t)
	wd := t.TempDir()
	rw := filepath.Join(wd, "backend")
	docs := filepath.Join(wd, "docs")
	for _, d := range []string{rw, docs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	profile := b.ProfileFor([]string{rw}, denyAll())

	ok := filepath.Join(rw, "candidate.txt")
	if res, _ := b.Launch(profile, containment.LaunchRequest{Command: []string{touch, ok}, WorkingRoot: wd}); res.ExitCode != 0 {
		t.Fatalf("granted-root write should land, rc=%d", res.ExitCode)
	}
	if _, err := os.Stat(ok); err != nil {
		t.Fatal("granted-root file missing")
	}

	denied := filepath.Join(docs, "nope.txt")
	if res, _ := b.Launch(profile, containment.LaunchRequest{Command: []string{touch, denied}, WorkingRoot: wd}); res.ExitCode == 0 {
		t.Fatal("ungranted sibling must be denied")
	}
	if _, err := os.Stat(denied); !os.IsNotExist(err) {
		t.Fatal("ungranted file must not exist")
	}

	unbound := filepath.Join(wd, "stray.txt")
	if res, _ := b.Launch(profile, containment.LaunchRequest{Command: []string{touch, unbound}, WorkingRoot: wd}); res.ExitCode == 0 {
		t.Fatal("unbound in-workdir path must be denied")
	}

	if err := b.Probe(profile, wd, []string{rw}); err != nil {
		t.Fatalf("probe should accept the per-binding profile: %v", err)
	}
}

func TestLandlock_ProbePassesForConformant(t *testing.T) {
	b := requireLandlock(t)
	wd := t.TempDir()
	if err := b.Probe(b.ProfileFor([]string{wd}, allowAll()), wd, []string{wd}); err != nil {
		t.Fatalf("permissive probe: %v", err)
	}
	if err := b.Probe(b.ProfileFor(nil, denyAll()), wd, nil); err != nil {
		t.Fatalf("readonly probe: %v", err)
	}
}

func TestLandlock_ProbeCatchesMisloweredWritable(t *testing.T) {
	b := requireLandlock(t)
	wd := t.TempDir()
	// BUG SIM: a ReadOnly run handed a Permissive-style writable-WORKDIR profile.
	mislowered := b.ProfileFor([]string{wd}, allowAll())
	if err := b.Probe(mislowered, wd, nil); !errors.Is(err, containment.ErrPaddockNotEstablished) {
		t.Fatalf("want ErrPaddockNotEstablished, got %v", err)
	}
}

func TestLandlock_TrampolineDispatch(t *testing.T) {
	if _, handled := MaybeRunTrampoline([]string{"prog"}); handled {
		t.Fatal("no sentinel: must not be handled")
	}
	if _, handled := MaybeRunTrampoline([]string{"prog", "--flag"}); handled {
		t.Fatal("non-sentinel arg: must not be handled")
	}
	rc, handled := MaybeRunTrampoline([]string{"prog", ExecSentinel})
	if !handled {
		t.Fatal("ExecSentinel must be handled")
	}
	if rc != ConfineFailedRC {
		t.Fatalf("missing roots argv must fail closed, rc=%d", rc)
	}
}

func TestLaunchConfined_ProbesFirst(t *testing.T) {
	b := requireLandlock(t)
	touch := requireTouch(t)
	wd := t.TempDir()
	res, err := containment.LaunchConfined(b, containment.PermissiveFor(wd),
		containment.LaunchRequest{Command: []string{touch, filepath.Join(wd, "ok.txt")}, WorkingRoot: wd})
	if err != nil {
		t.Fatalf("LaunchConfined: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("rc=%d stderr=%s", res.ExitCode, res.Stderr)
	}
	if _, err := os.Stat(filepath.Join(wd, "ok.txt")); err != nil {
		t.Fatal("confined write should have landed")
	}
}

func TestDetect_MatchesAvailability(t *testing.T) {
	ok, _ := New().Available()
	got := Detect()
	if ok && got == nil {
		t.Fatal("Detect must return a backend when landlock is available")
	}
	if !ok && got != nil {
		t.Fatal("Detect must return nil when landlock is unavailable")
	}
	if got != nil && got.Name() != "landlock" {
		t.Fatalf("Detect name = %q", got.Name())
	}
}

func TestLandlock_DeniedClassified(t *testing.T) {
	b := requireLandlock(t)
	touch := requireTouch(t)
	wd := t.TempDir()
	profile := b.ProfileFor([]string{wd}, allowAll())
	outside := filepath.Join(filepath.Dir(wd), "escape.txt")
	_ = os.Remove(outside)

	res, err := b.Launch(profile, containment.LaunchRequest{Command: []string{touch, outside}, WorkingRoot: wd})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 {
		t.Fatal("expected a denial")
	}
	if !containment.DeniedExit(res) {
		t.Fatalf("denial not classified from stderr=%q", res.Stderr)
	}
}

// --- Network pinning (Plan 4 Phase 3) ---

func TestLandlock_SupportsNetwork(t *testing.T) {
	b := requireLandlock(t)
	version, err := abi()
	if err != nil {
		t.Fatal(err)
	}
	want := version >= minABINetwork
	if got := b.SupportsNetwork(containment.NetDenyAll); got != want {
		t.Fatalf("SupportsNetwork(deny_all) = %v, want %v (ABI %d)", got, want, version)
	}
	if !b.SupportsNetwork(containment.NetAllowAll) {
		t.Fatal("allow_all must always be supported")
	}
	if b.SupportsNetwork(containment.NetMode("bogus")) {
		t.Fatal("an unknown mode must not be supported")
	}
}

func TestLandlock_NetworkDenyAll(t *testing.T) {
	b := requireLandlock(t)
	if !b.SupportsNetwork(containment.NetDenyAll) {
		t.Skip("kernel lacks Landlock network control (ABI < 4)")
	}
	wd := t.TempDir()
	profile := b.ProfileFor([]string{wd}, denyAll())
	denied, err := b.netDenied(profile, wd, net.JoinHostPort("127.0.0.1", "1"))
	if err != nil {
		t.Fatal(err)
	}
	if !denied {
		t.Fatal("deny_all must deny a TCP connect at the syscall")
	}
}

func TestLandlock_NetworkPinned(t *testing.T) {
	b := requireLandlock(t)
	if !b.SupportsNetwork(containment.NetBroker) {
		t.Skip("kernel lacks Landlock network control (ABI < 4)")
	}
	wd := t.TempDir()

	// A loopback listener stands in for the broker.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	profile := b.ProfileFor([]string{wd}, containment.NetworkPolicy{Mode: containment.NetBroker, BrokerPort: port})

	if denied, err := b.netDenied(profile, wd, net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err != nil {
		t.Fatal(err)
	} else if denied {
		t.Fatalf("the pinned egress port %d must be admitted", port)
	}

	other := "1"
	if port == 1 {
		other = "2"
	}
	if denied, err := b.netDenied(profile, wd, net.JoinHostPort("127.0.0.1", other)); err != nil {
		t.Fatal(err)
	} else if !denied {
		t.Fatalf("the non-pinned port %s must be denied", other)
	}
}

func TestLandlock_ProbeNetwork(t *testing.T) {
	b := requireLandlock(t)
	if !b.SupportsNetwork(containment.NetDenyAll) {
		t.Skip("kernel lacks Landlock network control (ABI < 4)")
	}
	wd := t.TempDir()
	if err := b.Probe(b.ProfileFor([]string{wd}, denyAll()), wd, []string{wd}); err != nil {
		t.Fatalf("deny_all probe: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	if err := b.Probe(b.ProfileFor([]string{wd}, containment.NetworkPolicy{Mode: containment.NetBroker, BrokerPort: port}), wd, []string{wd}); err != nil {
		t.Fatalf("broker probe: %v", err)
	}
}

// TestLandlock_InvokeBoundedByDefaultCeiling pins the invoke ceiling: a
// Launch whose request carries no timeout must not hang indefinitely. The
// ceiling is shrunk for the test and the killed run surfaces as the context
// deadline error rather than a bogus exit code.
func TestLandlock_InvokeBoundedByDefaultCeiling(t *testing.T) {
	b := requireLandlock(t)
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep unavailable: %v", err)
	}
	wd := t.TempDir()
	profile := b.ProfileFor([]string{wd}, allowAll())

	old := execCeiling
	execCeiling = 300 * time.Millisecond
	t.Cleanup(func() { execCeiling = old })

	_, err = b.Launch(profile, containment.LaunchRequest{Command: []string{sleep, "5"}, WorkingRoot: wd})
	if err == nil {
		t.Fatal("an unbounded launch must be bounded by the default ceiling")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}
