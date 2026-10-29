//go:build darwin

package seatbelt

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/buchenberg/goatherd-runtime/sandbox/confinement"
	"github.com/buchenberg/goatherd-runtime/sandbox/containment"
)

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	return confinement.Canonical(t.TempDir())
}

func launchConfined(t *testing.T, spec confinement.ConfinementSpec, wd string, command ...string) containment.LaunchResult {
	t.Helper()
	result, err := containment.LaunchConfined(New(), spec, containment.LaunchRequest{
		Command:     command,
		WorkingRoot: wd,
		Timeout:     30 * time.Second,
	})
	if err != nil {
		t.Fatalf("confined launch: %v", err)
	}
	return result
}

func listenPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

func nc(port int) []string {
	return []string{"/usr/bin/nc", "-z", "-w", "1", "127.0.0.1", strconv.Itoa(port)}
}

func TestSeatbelt_Available(t *testing.T) {
	ok, why := New().Available()
	if !ok {
		t.Fatalf("the Seatbelt backend is unavailable on macOS: %s", why)
	}
}

func TestSeatbelt_Detect(t *testing.T) {
	if b := Detect(); b == nil {
		t.Fatal("Detect returned nil on macOS")
	}
}

func TestSeatbelt_LaunchDeniesOutOfWorkdirWrite(t *testing.T) {
	wd := canonicalTempDir(t)
	spec := confinement.PermissiveFor(wd)

	inside := filepath.Join(wd, "ok.txt")
	if res := launchConfined(t, spec, wd, "/usr/bin/touch", inside); res.ExitCode != 0 {
		t.Fatalf("an in-WORKDIR write exited %d: %s", res.ExitCode, res.Stderr)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Fatalf("the in-WORKDIR write did not land: %v", err)
	}

	outside := filepath.Join(filepath.Dir(wd), ".seatbelt-escape")
	_ = os.Remove(outside)
	t.Cleanup(func() { _ = os.Remove(outside) })
	if res := launchConfined(t, spec, wd, "/usr/bin/touch", outside); res.ExitCode == 0 {
		t.Fatalf("an out-of-WORKDIR write was permitted: %+v", res)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("the out-of-WORKDIR write reached disk")
	}
}

func TestSeatbelt_ReadOnlyDeniesInWorkdirWrite(t *testing.T) {
	wd := canonicalTempDir(t)
	target := filepath.Join(wd, "nope.txt")
	res := launchConfined(t, confinement.ReadOnlySpec(), wd, "/usr/bin/touch", target)
	if res.ExitCode == 0 {
		t.Fatalf("a ReadOnly in-WORKDIR write was permitted: %+v", res)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("a ReadOnly in-WORKDIR write reached disk")
	}
}

func TestSeatbelt_PerBindingDenyClosed(t *testing.T) {
	wd := canonicalTempDir(t)
	backendDir := filepath.Join(wd, "backend")
	docsDir := filepath.Join(wd, "docs")
	if err := os.MkdirAll(backendDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := confinement.ConfinementSpec{
		WritableRoots: []string{backendDir},
		Network:       confinement.NetworkPolicy{Mode: confinement.NetAllowAll},
	}

	ok := filepath.Join(backendDir, "candidate.txt")
	if res := launchConfined(t, spec, wd, "/usr/bin/touch", ok); res.ExitCode != 0 {
		t.Fatalf("a write beneath the granted root exited %d: %s", res.ExitCode, res.Stderr)
	}
	if _, err := os.Stat(ok); err != nil {
		t.Fatalf("the granted-root write did not land: %v", err)
	}

	for _, target := range []string{
		filepath.Join(docsDir, "nope.txt"),
		filepath.Join(wd, "stray.txt"),
	} {
		if res := launchConfined(t, spec, wd, "/usr/bin/touch", target); res.ExitCode == 0 {
			t.Fatalf("a write to %s was permitted: %+v", target, res)
		}
		if _, err := os.Stat(target); err == nil {
			t.Fatalf("the write to %s reached disk", target)
		}
	}
}

func TestSeatbelt_GitWritesDenied(t *testing.T) {
	wd := canonicalTempDir(t)
	gitDir := filepath.Join(wd, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := confinement.PermissiveFor(wd)

	target := filepath.Join(gitDir, "config")
	res := launchConfined(t, spec, wd, "/usr/bin/touch", target)
	if res.ExitCode == 0 {
		t.Fatalf("a write beneath .git was permitted: %+v", res)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("a write beneath .git reached disk")
	}

	// The rest of the writable root is still writable.
	if res := launchConfined(t, spec, wd, "/usr/bin/touch", filepath.Join(wd, "ok.txt")); res.ExitCode != 0 {
		t.Fatalf("a normal workspace write exited %d: %s", res.ExitCode, res.Stderr)
	}
}

func TestSeatbelt_ProbeRejectsNonJailingProfile(t *testing.T) {
	wd := canonicalTempDir(t)
	err := New().Probe("(version 1)\n(allow default)\n", wd, []string{wd})
	if !errors.Is(err, containment.ErrPaddockNotEstablished) {
		t.Fatalf("Probe = %v, want ErrPaddockNotEstablished", err)
	}
}

func TestSeatbelt_ProbeCatchesReadOnlyMisLoweredToWritable(t *testing.T) {
	wd := canonicalTempDir(t)
	b := New()
	misLowered := b.ProfileFor([]string{wd}, confinement.NetworkPolicy{Mode: confinement.NetAllowAll})
	err := b.Probe(misLowered, wd, nil)
	if !errors.Is(err, containment.ErrPaddockNotEstablished) {
		t.Fatalf("Probe = %v, want ErrPaddockNotEstablished", err)
	}
}

func TestSeatbelt_ProbePassesConformantProfiles(t *testing.T) {
	wd := canonicalTempDir(t)
	b := New()

	permissive := b.ProfileFor([]string{wd}, confinement.NetworkPolicy{Mode: confinement.NetAllowAll})
	if err := b.Probe(permissive, wd, []string{wd}); err != nil {
		t.Fatalf("Permissive probe: %v", err)
	}

	readOnly := b.ProfileFor(nil, confinement.NetworkPolicy{Mode: confinement.NetDenyAll})
	if err := b.Probe(readOnly, wd, nil); err != nil {
		t.Fatalf("ReadOnly probe: %v", err)
	}
}

func TestSeatbelt_HandlesMetacharacterWorkspace(t *testing.T) {
	wd := filepath.Join(t.TempDir(), `ws a'b"c`)
	if err := os.MkdirAll(wd, 0o755); err != nil {
		t.Fatal(err)
	}
	wd = confinement.Canonical(wd)
	spec := confinement.PermissiveFor(wd)
	target := filepath.Join(wd, "ok.txt")
	if res := launchConfined(t, spec, wd, "/usr/bin/touch", target); res.ExitCode != 0 {
		t.Fatalf("a write in a metacharacter workspace exited %d: %s", res.ExitCode, res.Stderr)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the write did not land: %v", err)
	}
}

func TestSeatbelt_NetworkDenyAll(t *testing.T) {
	wd := canonicalTempDir(t)
	port := listenPort(t)
	spec := confinement.ConfinementSpec{Network: confinement.NetworkPolicy{Mode: confinement.NetDenyAll}}
	res := launchConfined(t, spec, wd, nc(port)...)
	if res.ExitCode == 0 {
		t.Fatalf("a deny_all connect to a live local port was permitted: %+v", res)
	}
}

func TestSeatbelt_NetworkBrokerPinsLoopbackPort(t *testing.T) {
	wd := canonicalTempDir(t)
	pinned := listenPort(t)
	other := listenPort(t)
	spec := confinement.ConfinementSpec{Network: confinement.NetworkPolicy{
		Mode:       confinement.NetBroker,
		BrokerPort: pinned,
	}}

	if res := launchConfined(t, spec, wd, nc(pinned)...); res.ExitCode != 0 {
		t.Fatalf("a connect to the pinned port exited %d: %s", res.ExitCode, res.Stderr)
	}
	if res := launchConfined(t, spec, wd, nc(other)...); res.ExitCode == 0 {
		t.Fatalf("a connect to a non-pinned port was permitted: %+v", res)
	}
}
