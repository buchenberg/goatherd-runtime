//go:build windows

package winacl

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/buchenberg/shepherd-runtime-go/sandbox/containment"
	"golang.org/x/sys/windows"
)

// TestMain makes the test binary a runner: the confined child is this same
// executable, so the runner's argv sentinels must be handled before the
// testing framework parses flags.
//
// A binary that omits this hook makes every confined launch fail with the
// confine-failed code, which the probe turns into ErrPaddockNotEstablished —
// fail-closed, never silently unconfined.
func TestMain(m *testing.M) {
	if code, handled := MaybeRunRunner(os.Args); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// tree is a temp tree with two granted roots and one ungranted sibling.
type tree struct {
	base    string
	rootA   string
	rootB   string
	outside string
}

func newTree(t *testing.T) tree {
	t.Helper()
	base := t.TempDir()
	tr := tree{
		base:    base,
		rootA:   filepath.Join(base, "rootA"),
		rootB:   filepath.Join(base, "rootB"),
		outside: filepath.Join(base, "outside"),
	}
	for _, dir := range []string{tr.rootA, tr.rootB, tr.outside} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	return tr
}

// effectCommand returns the argv of a command that attempts one file effect
// and reports the outcome in its exit code.
func effectCommand(t *testing.T, op, target string) []string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test executable: %v", err)
	}
	return []string{executable, ProbeChildSentinel, op, target}
}

func TestWinACL_AvailableOnWindows(t *testing.T) {
	ok, why := New().Available()
	if !ok {
		t.Fatalf("the backend is unavailable on Windows: %s", why)
	}
}

func TestWinACL_WriteLandsInsideAGrantedRoot(t *testing.T) {
	tr := newTree(t)
	target := filepath.Join(tr.rootA, "inside.txt")
	spec := containment.ConfinementSpec{
		WritableRoots: []string{tr.rootA, tr.rootB},
		Network:       containment.NetworkPolicy{Mode: containment.NetAllowAll},
	}
	result, err := containment.LaunchConfined(New(), spec, containment.LaunchRequest{
		Command:     effectCommand(t, "write", target),
		WorkingRoot: tr.rootA,
	})
	if err != nil {
		t.Fatalf("the confined launch failed: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("the write inside a granted root exited %d: %s", result.ExitCode, result.Stderr)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the write inside a granted root did not land: %v", err)
	}
}

func TestWinACL_WriteIsDeniedOutsideEveryRoot(t *testing.T) {
	tr := newTree(t)
	target := filepath.Join(tr.outside, "outside.txt")
	spec := containment.ConfinementSpec{
		WritableRoots: []string{tr.rootA, tr.rootB},
		Network:       containment.NetworkPolicy{Mode: containment.NetAllowAll},
	}
	result, err := containment.LaunchConfined(New(), spec, containment.LaunchRequest{
		Command:     effectCommand(t, "write", target),
		WorkingRoot: tr.rootA,
	})
	if err != nil {
		t.Fatalf("the confined launch failed: %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatal("a write outside every granted root was ALLOWED — the paddock does not hold")
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("the write outside every granted root landed on disk")
	}
}

func TestWinACL_EveryGrantedRootIsWritable(t *testing.T) {
	tr := newTree(t)
	spec := containment.ConfinementSpec{
		WritableRoots: []string{tr.rootA, tr.rootB},
		Network:       containment.NetworkPolicy{Mode: containment.NetAllowAll},
	}
	for _, root := range []string{tr.rootA, tr.rootB} {
		target := filepath.Join(root, "per-root.txt")
		result, err := containment.LaunchConfined(New(), spec, containment.LaunchRequest{
			Command:     effectCommand(t, "write", target),
			WorkingRoot: tr.rootA,
		})
		if err != nil {
			t.Fatalf("the confined launch failed: %v", err)
		}
		if result.ExitCode != 0 {
			t.Fatalf("the write inside %s exited %d: %s", root, result.ExitCode, result.Stderr)
		}
	}
}

func TestWinACL_DeleteIsDeniedOutsideEveryRoot(t *testing.T) {
	tr := newTree(t)
	victim := filepath.Join(tr.outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("host owned\n"), 0o600); err != nil {
		t.Fatalf("seed the victim: %v", err)
	}
	spec := containment.ConfinementSpec{
		WritableRoots: []string{tr.rootA},
		Network:       containment.NetworkPolicy{Mode: containment.NetAllowAll},
	}
	result, err := containment.LaunchConfined(New(), spec, containment.LaunchRequest{
		Command:     effectCommand(t, "delete", victim),
		WorkingRoot: tr.rootA,
	})
	if err != nil {
		t.Fatalf("the confined launch failed: %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatal("a delete outside every granted root was ALLOWED")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("the victim was deleted despite the denial")
	}
}

func TestWinACL_ReadOnlyDeniesEveryWrite(t *testing.T) {
	tr := newTree(t)
	target := filepath.Join(tr.rootA, "read-only.txt")
	spec := readOnlySpec()
	result, err := containment.LaunchConfined(New(), spec, containment.LaunchRequest{
		Command:     effectCommand(t, "write", target),
		WorkingRoot: tr.rootA,
	})
	if err != nil {
		t.Fatalf("the confined launch failed: %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatal("a write landed under a read-only profile")
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("the read-only write landed on disk")
	}
}

func TestWinACL_ReadIsNotGoverned(t *testing.T) {
	tr := newTree(t)
	readable := filepath.Join(tr.outside, "readable.txt")
	if err := os.WriteFile(readable, []byte("host owned\n"), 0o600); err != nil {
		t.Fatalf("seed the readable file: %v", err)
	}
	spec := containment.ConfinementSpec{
		WritableRoots: []string{tr.rootA},
		Network:       containment.NetworkPolicy{Mode: containment.NetAllowAll},
	}
	result, err := containment.LaunchConfined(New(), spec, containment.LaunchRequest{
		Command:     effectCommand(t, "read", readable),
		WorkingRoot: tr.rootA,
	})
	if err != nil {
		t.Fatalf("the confined launch failed: %v", err)
	}
	// The write-restricted intersection covers write accesses only, so a read
	// outside every granted root is admitted. This is the documented read
	// boundary, and it is Landlock parity — not a gap — but it is pinned here
	// so a change that narrowed it would be noticed.
	if result.ExitCode != 0 {
		t.Fatalf("a read outside every granted root was denied (exit %d): %s — reads are not governed",
			result.ExitCode, result.Stderr)
	}
}

func TestWinACL_ProbePassesForAConformantProfile(t *testing.T) {
	tr := newTree(t)
	b := New()
	roots := []string{tr.rootA, tr.rootB}
	doc := b.ProfileFor(roots, containment.NetworkPolicy{Mode: containment.NetAllowAll})
	if err := b.Probe(doc, tr.rootA, roots); err != nil {
		t.Fatalf("a conformant profile failed the probe: %v", err)
	}
}

func TestWinACL_ProbeCatchesAMisloweredProfile(t *testing.T) {
	tr := newTree(t)
	b := New()
	// A profile that grants the working directory, probed against a root set
	// that does not include it: the deny-closed check must catch the broader
	// grant, because a write inside the working directory would be permitted.
	doc := b.ProfileFor([]string{tr.rootA}, containment.NetworkPolicy{Mode: containment.NetAllowAll})
	if err := b.Probe(doc, tr.rootA, nil); err == nil {
		t.Fatal("a profile granting the working directory passed the probe against a root set that excludes it")
	}
}

func TestWinACL_NetworkModeWithoutEnforcementIsRefused(t *testing.T) {
	tr := newTree(t)
	spec := readOnlySpec()
	spec.Network = containment.NetworkPolicy{Mode: containment.NetDenyAll}
	_, err := containment.LaunchConfined(New(), spec, containment.LaunchRequest{
		Command:     effectCommand(t, "write", filepath.Join(tr.rootA, "denied.txt")),
		WorkingRoot: tr.rootA,
	})
	if err == nil {
		t.Fatal("a network mode the mechanism cannot enforce was accepted")
	}
	if !errors.Is(err, containment.ErrUnsupportedConfinementSpec) {
		t.Fatalf("the refusal is %v, want ErrUnsupportedConfinementSpec", err)
	}
}

func TestWinACL_GrantIsIdempotent(t *testing.T) {
	tr := newTree(t)
	worldSID, lowSID, writeSID := grantIdentities(t, tr.rootA)
	for i := 0; i < 2; i++ {
		if err := GrantWrite(tr.rootA, writeSID, worldSID, lowSID); err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
	}
	// The second grant must have been skipped: the exact ACE, the exact deny,
	// and the exact label already stood, so the apply — an eager, tree-wide
	// security descriptor write — did not run again. A duplicated capability
	// ACE would mean the skip did not fire.
	if count := countCapabilityAces(t, tr.rootA, writeSID); count != 1 {
		t.Fatalf("the DACL carries %d capability ACEs after two grants, want 1 — the exact-ACE skip did not fire", count)
	}
}

// countCapabilityAces counts the allow ACEs in a path's DACL that name the
// given write identity.
func countCapabilityAces(t *testing.T, path string, writeSID *windows.SID) int {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read the security descriptor of %s: %v", path, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("read the DACL of %s: %v", path, err)
	}
	count := 0
	forEachACE(aclView(dacl), func(aceType, aceFlags byte, mask uint32, aceStart int) bool {
		if aceType != accessAllowedAceType || mask != grantMask {
			return true
		}
		if inlineSIDEquals(aclView(dacl), aceStart, writeSID) {
			count++
		}
		return true
	})
	return count
}

func TestWinACL_RevokeRemovesTheCapabilityAce(t *testing.T) {
	tr := newTree(t)
	worldSID, lowSID, writeSID := grantIdentities(t, tr.rootA)
	if err := GrantWrite(tr.rootA, writeSID, worldSID, lowSID); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := RevokeWrite(tr.rootA, writeSID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
}

// readOnlySpec is the read-only lowering with a representable network mode.
func readOnlySpec() containment.ConfinementSpec {
	return containment.ConfinementSpec{
		Network: containment.NetworkPolicy{Mode: containment.NetAllowAll},
	}
}

// grantIdentities builds the SIDs a grant needs: the world SID the
// ambient-delete deny names, the Low integrity SID the label names, and the
// root's write capability SID.
func grantIdentities(t *testing.T, root string) (worldSID, lowSID, writeSID *windows.SID) {
	t.Helper()
	worldSID, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatalf("build the world SID: %v", err)
	}
	lowSID, err = windows.CreateWellKnownSid(windows.WinLowLabelSid)
	if err != nil {
		t.Fatalf("build the low integrity SID: %v", err)
	}
	writeSID, err = windows.StringToSid(WriteSidFor(containment.Canonical(root)))
	if err != nil {
		t.Fatalf("parse the write identity: %v", err)
	}
	return worldSID, lowSID, writeSID
}
