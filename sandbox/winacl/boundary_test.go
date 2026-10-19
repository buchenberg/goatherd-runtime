//go:build windows

package winacl

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buchenberg/shepherd-runtime-go/sandbox/containment"
	"golang.org/x/sys/windows"
	"unsafe"
)

// TestWinACL_DirectoryCanBeCreatedInsideAGrantedRoot pins the capability's
// access mask.
//
// FILE_APPEND_DATA is FILE_ADD_SUBDIRECTORY on a directory, and a write mask
// that omits it refuses a directory creation inside the granted root — which
// is a routine thing for a build to do, and a confusing refusal when it
// happens.
func TestWinACL_DirectoryCanBeCreatedInsideAGrantedRoot(t *testing.T) {
	tr := newTree(t)
	spec := containment.ConfinementSpec{
		WritableRoots: []string{tr.rootA},
		Network:       containment.NetworkPolicy{Mode: containment.NetAllowAll},
	}
	// Two levels, because a nested creation needs the intermediate directory
	// to exist first.
	for _, target := range []string{
		filepath.Join(tr.rootA, "created"),
		filepath.Join(tr.rootA, "created", "sub"),
	} {
		result, err := containment.LaunchConfined(New(), spec, containment.LaunchRequest{
			Command:     effectCommand(t, "mkdir", target),
			WorkingRoot: tr.rootA,
		})
		if err != nil {
			t.Fatalf("the confined launch failed: %v", err)
		}
		if result.ExitCode != 0 {
			t.Fatalf("a directory creation inside a granted root exited %d: %s", result.ExitCode, result.Stderr)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("the directory was not created: %v", err)
		}
	}
}

// TestWinACL_EnvironmentReachesTheChild proves the request's environment
// reaches the confined child.
//
// The environment block is built by the runner, and the runner is given the
// request's environment: a block that terminated early would leave the child
// with an empty environment and no PATH.
func TestWinACL_EnvironmentReachesTheChild(t *testing.T) {
	tr := newTree(t)
	target := filepath.Join(tr.rootA, "env.txt")
	spec := containment.ConfinementSpec{
		WritableRoots: []string{tr.rootA},
		Network:       containment.NetworkPolicy{Mode: containment.NetAllowAll},
	}
	env := append(os.Environ(), envProbeVariable+"=carried-through")
	result, err := containment.LaunchConfined(New(), spec, containment.LaunchRequest{
		Command:     effectCommand(t, "writeenv", target),
		WorkingRoot: tr.rootA,
		Env:         env,
	})
	if err != nil {
		t.Fatalf("the confined launch failed: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("the environment probe exited %d: %s", result.ExitCode, result.Stderr)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read the environment probe file: %v", err)
	}
	if strings.TrimSpace(string(content)) != "carried-through" {
		t.Fatalf("the child saw the environment value %q, want %q", strings.TrimSpace(string(content)), "carried-through")
	}
}

// TestWinACL_ChildStandardOutputReachesTheRunner proves the child's standard
// output is not swallowed.
//
// A confined body whose output is captured by the runtime is the whole point
// of the backend; a child that starts with no standard handles is mute, and
// the runtime would record an empty result for a body that worked.
func TestWinACL_ChildStandardOutputReachesTheRunner(t *testing.T) {
	tr := newTree(t)
	spec := containment.ConfinementSpec{
		WritableRoots: []string{tr.rootA},
		Network:       containment.NetworkPolicy{Mode: containment.NetAllowAll},
	}
	result, err := containment.LaunchConfined(New(), spec, containment.LaunchRequest{
		Command:     effectCommand(t, "echo", ""),
		WorkingRoot: tr.rootA,
	})
	if err != nil {
		t.Fatalf("the confined launch failed: %v", err)
	}
	if !strings.Contains(result.Stdout, "winacl-stdout") {
		t.Fatalf("the child's standard output was %q, want it to contain %q", result.Stdout, "winacl-stdout")
	}
}

// TestWinACL_CommandIsResolvedThroughPath proves an ordinary command name
// works.
//
// CreateProcessAsUser does not search PATH when the application name is
// given, so a body launched as "git" or "pwsh" would fail unless the runner
// resolves it.
func TestWinACL_CommandIsResolvedThroughPath(t *testing.T) {
	if _, err := exec.LookPath("cmd.exe"); err != nil {
		t.Skip("cmd.exe is not on PATH")
	}
	tr := newTree(t)
	spec := containment.ConfinementSpec{
		WritableRoots: []string{tr.rootA},
		Network:       containment.NetworkPolicy{Mode: containment.NetAllowAll},
	}
	result, err := containment.LaunchConfined(New(), spec, containment.LaunchRequest{
		Command:     []string{"cmd.exe", "/c", "exit 0"},
		WorkingRoot: tr.rootA,
	})
	if err != nil {
		t.Fatalf("the confined launch failed: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("a PATH-resolved command exited %d: %s", result.ExitCode, result.Stderr)
	}
}

// TestWinACL_ReadOnlyProfileWithRootsIsRefused proves a malformed profile is
// refused rather than crashing the runner.
//
// A profile arrives as JSON on the runner's argv, so it is untrusted input: a
// read-only profile that names roots it does not carry a write identity for
// must be an error, not an index-out-of-range in the runner.
func TestWinACL_ReadOnlyProfileWithRootsIsRefused(t *testing.T) {
	prof := profile{Roots: []string{`C:\work\a`}, Mode: modeReadOnly}
	if err := prof.validate(); err == nil {
		t.Fatal("a read-only profile naming roots was accepted")
	}
}

// TestWinACL_DefaultDaclIsMergedNotReplaced proves the token's default DACL
// keeps the trustees it already had.
//
// A replacement would name only the capability SID — a SID that is in the
// restricting list but NOT in the token's normal SID set — so the child could
// create an object and then fail the normal access check when it reopened it.
// The merge has to preserve the original ACL and add to it.
//
// This is also the test that catches a mis-read of the TOKEN_DEFAULT_DACL
// structure: the buffer holds a pointer to the ACL, not the ACL, so reading
// the buffer's own bytes as an ACL header yields a descriptor made of the
// pointer's bytes, and the merge silently becomes a replacement.
//
// The token it mutates is one the test OWNS — a restricted child token. The
// process's own primary token is not touched: setting a default DACL on it
// would be a process-wide, irreversible change that every later object and
// derived token would inherit, making the suite order-dependent.
func TestWinACL_DefaultDaclIsMergedNotReplaced(t *testing.T) {
	current, err := openProcessToken()
	if err != nil {
		t.Fatalf("open the process token: %v", err)
	}
	defer current.Close()
	logon, err := logonSID(current)
	if err != nil {
		t.Fatalf("the logon SID: %v", err)
	}
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatalf("the world SID: %v", err)
	}
	owned, err := createRestrictedTokenWith(current, []windows.SIDAndAttributes{{Sid: logon}, {Sid: world}})
	if err != nil {
		t.Fatalf("mint a restricted token: %v", err)
	}
	defer owned.Close()

	before, keepBefore, err := tokenDefaultDacl(owned)
	if err != nil {
		t.Fatalf("read the token default DACL: %v", err)
	}
	defer keepBefore()
	if before == nil {
		t.Skip("the token carries no default DACL to merge into")
	}
	beforeAces := countAces(aclView(before))

	// Merge an ACE for the world SID into it.
	if err := setDefaultDacl(owned, world); err != nil {
		t.Fatalf("merge into the token default DACL: %v", err)
	}
	after, keepAfter, err := tokenDefaultDacl(owned)
	if err != nil {
		t.Fatalf("read the merged token default DACL: %v", err)
	}
	defer keepAfter()
	if after == nil {
		t.Fatal("the merged default DACL is gone")
	}
	afterAces := countAces(aclView(after))
	if afterAces <= beforeAces {
		t.Fatalf("the default DACL has %d ACEs after the merge and %d before — it was replaced, not merged",
			afterAces, beforeAces)
	}
}

// countAces counts the ACEs in an ACL view.
func countAces(view []byte) int {
	count := 0
	forEachACE(view, func(aceType, aceFlags byte, mask uint32, aceStart int) bool {
		count++
		return true
	})
	return count
}

// With no restricting SIDs at all, every access check is refused: the child
// starts, but it cannot open a file, so nothing is writable. The write
// allowlist is therefore entirely the restricting list — nothing about the
// grant, the label, or the ambient user rights authorizes a write on their
// own. And the list must name the capability SID: a list of the keep-alive
// SIDs alone leaves the granted root unwritable.
//
// Which keep-alive SIDs the list must carry is a separate question, answered
// by TestWinACL_CngBodyNeedsBothKeepAliveSids.
func TestWinACL_RestrictingListIsTheWholeBasisOfAWrite(t *testing.T) {
	tr := newTree(t)
	worldSID, lowSID, writeSID := grantIdentities(t, tr.rootA)
	if err := GrantWrite(tr.rootA, writeSID, worldSID, lowSID); err != nil {
		t.Fatalf("grant: %v", err)
	}
	current, err := openProcessToken()
	if err != nil {
		t.Fatalf("open the process token: %v", err)
	}
	defer current.Close()
	logon, err := logonSID(current)
	if err != nil {
		t.Fatalf("the logon SID: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test executable: %v", err)
	}

	// writeIntoRoot runs a confined write into the granted root and reports
	// whether it landed. Only a genuine denial (exit 1) counts as "denied" —
	// the confine-failed code is an infrastructure failure, and treating it
	// as a denial would let a broken harness satisfy the assertion.
	writeIntoRoot := func(restricting []windows.SIDAndAttributes) bool {
		t.Helper()
		token, err := createRestrictedTokenWith(current, restricting)
		if err != nil {
			t.Fatalf("mint the restricted token: %v", err)
		}
		defer token.Close()
		if err := lowerIntegrity(token, lowSID); err != nil {
			t.Fatalf("lower the integrity level: %v", err)
		}
		if err := setDefaultDacl(token, worldSID); err != nil {
			t.Fatalf("set the default DACL: %v", err)
		}
		target := filepath.Join(tr.rootA, "probe-"+fmt.Sprint(len(restricting))+".txt")
		switch rc := spawnAndWait(token, []string{executable, ProbeChildSentinel, "write", target}); rc {
		case 0:
			_, statErr := os.Stat(target)
			return statErr == nil
		case 1:
			return false
		default:
			t.Fatalf("the confined write exited %d, which is not a denial", rc)
			return false
		}
	}

	if !writeIntoRoot([]windows.SIDAndAttributes{{Sid: logon}, {Sid: worldSID}, {Sid: writeSID}}) {
		t.Fatal("the capability SID did not authorize a write into the granted root")
	}
	if writeIntoRoot(nil) {
		t.Fatal("a write landed with an empty restricting list — the list is not the basis of the restriction")
	}
	if writeIntoRoot([]windows.SIDAndAttributes{{Sid: logon}, {Sid: worldSID}}) {
		t.Fatal("a write landed without the capability SID in the restricting list")
	}
}

// TestWinACL_CngBodyNeedsBothKeepAliveSids pins the stronger requirement: a
// body that uses CNG needs BOTH keep-alive SIDs.
//
// A minimal Go child starts with either one, so the requirement is only
// visible through a CNG-using body — PowerShell here, which loads BCrypt.
func TestWinACL_CngBodyNeedsBothKeepAliveSids(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh.exe")
	if err != nil {
		t.Skip("pwsh.exe is not on PATH")
	}
	current, err := openProcessToken()
	if err != nil {
		t.Fatalf("open the process token: %v", err)
	}
	defer current.Close()
	logon, err := logonSID(current)
	if err != nil {
		t.Fatalf("the logon SID: %v", err)
	}
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatalf("the world SID: %v", err)
	}
	low, err := windows.CreateWellKnownSid(windows.WinLowLabelSid)
	if err != nil {
		t.Fatalf("the low integrity SID: %v", err)
	}
	run := func(restricting []windows.SIDAndAttributes) int {
		t.Helper()
		token, err := createRestrictedTokenWith(current, restricting)
		if err != nil {
			t.Fatalf("mint the restricted token: %v", err)
		}
		defer token.Close()
		if err := lowerIntegrity(token, low); err != nil {
			t.Fatalf("lower the integrity level: %v", err)
		}
		if err := setDefaultDacl(token, world); err != nil {
			t.Fatalf("set the default DACL: %v", err)
		}
		return spawnAndWait(token, []string{pwsh, "-NoProfile", "-Command", "exit 0"})
	}
	if rc := run([]windows.SIDAndAttributes{{Sid: logon}, {Sid: world}}); rc != 0 {
		t.Fatalf("PowerShell did not start with the full restricting list (exit %d)", rc)
	}
	if rc := run([]windows.SIDAndAttributes{{Sid: world}}); rc == 0 {
		t.Fatal("PowerShell started without the logon session SID")
	}
	if rc := run([]windows.SIDAndAttributes{{Sid: logon}}); rc == 0 {
		t.Fatal("PowerShell started without Everyone")
	}
}

// TestWinACL_EveryoneWritableLowLabeledDirectoryIsNotConfined pins the
// residual boundary of the mechanism.
//
// The write-restricted check consults the restricting SIDs, and Everyone is
// one of them — it has to be, or a body that uses CNG cannot start. So a
// directory that grants write to Everyone AND carries a Low integrity label
// is writable by a confined child even when it is not a granted root: the
// low-to-low write is not blocked by the integrity check, and Everyone
// satisfies the second access check.
//
// A default Windows install does not produce such a directory — Everyone
// holds no write over the user's trees, and nothing but this mechanism and
// other sandbox tools stamp a tree Low. A tree another sandbox tool has
// stamped Low is the case where the boundary is reachable.
//
// This test constructs that directory deliberately and asserts that the write
// LANDS. It is the boundary stated as a fact rather than left as an
// assumption, and it fails loudly if a future change closes the boundary (or
// if someone comes to believe it was already closed).
func TestWinACL_EveryoneWritableLowLabeledDirectoryIsNotConfined(t *testing.T) {
	tr := newTree(t)
	worldSID, lowSID, _ := grantIdentities(t, tr.rootA)

	adversarial := filepath.Join(tr.outside, "adversarial")
	if err := os.Mkdir(adversarial, 0o700); err != nil {
		t.Fatalf("create the adversarial directory: %v", err)
	}
	if err := grantEveryoneWriteAndLowLabel(adversarial, worldSID, lowSID); err != nil {
		t.Fatalf("build the adversarial directory: %v", err)
	}

	target := filepath.Join(adversarial, "not-confined.txt")
	spec := containment.ConfinementSpec{
		WritableRoots: []string{tr.rootA},
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
		t.Fatalf("the write was denied (exit %d) — the Everyone path is closed, "+
			"and this test's boundary statement is now wrong", result.ExitCode)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal("the write did not land, but the runner reported success")
	}
}

// grantEveryoneWriteAndLowLabel gives a directory an Everyone write ACE and a
// Low no-write-up label: the two properties that make it writable by a
// confined child that does not hold its capability.
func grantEveryoneWriteAndLowLabel(path string, worldSID, lowSID *windows.SID) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read the security descriptor: %w", err)
	}
	currentDacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read the DACL: %w", err)
	}
	entries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: fileWriteData | fileAppendData | fileWriteEA | fileWriteAttributes,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			MultipleTrusteeOperation: windows.NO_MULTIPLE_TRUSTEE,
			TrusteeForm:              windows.TRUSTEE_IS_SID,
			TrusteeType:              windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue:             windows.TrusteeValueFromSID(worldSID),
		},
	}}
	newDacl, err := windows.ACLFromEntries(entries, currentDacl)
	if err != nil {
		return fmt.Errorf("merge the Everyone write ACE: %w", err)
	}
	label, err := buildLabelACL(labelACLBytes(sd), lowSID)
	if err != nil {
		return fmt.Errorf("build the label: %w", err)
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION,
		nil, nil, newDacl, (*windows.ACL)(unsafe.Pointer(&label[0])))
}
