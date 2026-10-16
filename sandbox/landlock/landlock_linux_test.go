//go:build linux

package landlock

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/buchenberg/shepherd-runtime-go/sandbox/containment"
)

// TestMain installs the trampoline hook. Every confine-then-exec and confined
// write-probe re-execs this very test binary, so the hook must run before the
// test runner.
func TestMain(m *testing.M) {
	if code, handled := MaybeRunTrampoline(os.Args); handled {
		os.Exit(code)
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

func TestLandlock_ProfileForParity(t *testing.T) {
	b := requireLandlock(t)
	wd := t.TempDir()
	got := b.ProfileFor([]string{wd}, true)
	var roots []string
	if err := json.Unmarshal([]byte(got), &roots); err != nil {
		t.Fatalf("profile is not a JSON array: %v (%q)", err, got)
	}
	if len(roots) != 1 || roots[0] != containment.Canonical(wd) {
		t.Fatalf("profile roots = %v, want [%s]", roots, containment.Canonical(wd))
	}
	if empty := b.ProfileFor(nil, false); empty != "[]" {
		t.Fatalf("empty profile = %q, want []", empty)
	}
}

func TestLandlock_ConfineDeniesOutside(t *testing.T) {
	b := requireLandlock(t)
	touch := requireTouch(t)
	wd := t.TempDir()
	profile := b.ProfileFor([]string{wd}, true)

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
	profile := b.ProfileFor(nil, false)

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
	profile := b.ProfileFor([]string{rw}, false)

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
	if err := b.Probe(b.ProfileFor([]string{wd}, true), wd, []string{wd}); err != nil {
		t.Fatalf("permissive probe: %v", err)
	}
	if err := b.Probe(b.ProfileFor(nil, false), wd, nil); err != nil {
		t.Fatalf("readonly probe: %v", err)
	}
}

func TestLandlock_ProbeCatchesMisloweredWritable(t *testing.T) {
	b := requireLandlock(t)
	wd := t.TempDir()
	// BUG SIM: a ReadOnly run handed a Permissive-style writable-WORKDIR profile.
	mislowered := b.ProfileFor([]string{wd}, true)
	if err := b.Probe(mislowered, wd, nil); !errors.Is(err, containment.ErrJailNotEstablished) {
		t.Fatalf("want ErrJailNotEstablished, got %v", err)
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
	profile := b.ProfileFor([]string{wd}, true)
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
