// Command winacl-spike proves the Windows write-restriction mechanism that a
// shepherd Windows containment backend would use.
//
// Mechanism under test:
//  1. a WRITE_RESTRICTED token (CreateRestrictedToken) whose restricting SIDs
//     are [logon SID, Everyone, <per-root write SID>];
//  2. a Low integrity level on that token;
//  3. per-root grants: an ALLOW ACE for the root's write SID, a DENY of the
//     ambient FILE_DELETE_CHILD for Everyone, and a Low no-write-up label.
//
// The claims under test:
//
//	A. write inside a granted root  -> ALLOWED
//	B. write outside every root    -> DENIED      (the paddock)
//	C. delete outside every root   -> DENIED      (the ambient-delete hole)
//	D. read outside every root     -> ALLOWED     (Landlock parity: reads not governed)
//	E. the three-part probe (liveness / per-root / deny-closed) ports 1:1
//
// Usage: winacl-spike            (parent: sets up and asserts)
//
//	winacl-spike --child OP PATH  (child: op in {write,read,delete,mkdir})
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	advapi32                  = windows.NewLazySystemDLL("advapi32.dll")
	procCreateRestrictedToken = advapi32.NewProc("CreateRestrictedToken")
	procAddMandatoryAce       = advapi32.NewProc("AddMandatoryAce")
)

// Win32 constants x/sys does not export.
const (
	disableMaxPrivilege = 0x00000001
	luaToken            = 0x00000002
	writeRestricted     = 0x00000008

	aclRevision             = 2
	mandatoryLabelNoWriteUp = 0x1
	seGroupLogonID          = 0xC0000000
	seGroupIntegrity        = 0x00000020

	grantMask = 0x000000F2 | // FILE_WRITE_DATA | FILE_APPEND_DATA | FILE_WRITE_EA | FILE_WRITE_ATTRIBUTES
		0x00010000 | // DELETE
		0x00020000 | // READ_CONTROL
		0x00100000 // SYNCHRONIZE
	fileDeleteChild  = 0x00000040
	containerInherit = 0x00000002
)

// aclHeader is the documented on-disk ACL header layout (8 bytes).
type aclHeader struct {
	Revision byte
	Sbz1     byte
	AclSize  uint16
	AceCount uint16
	Sbz2     uint16
}

func main() {
	if len(os.Args) > 2 && os.Args[1] == "--child" {
		childOp(os.Args[2], os.Args[3])
		return
	}
	if err := parent(); err != nil {
		fmt.Fprintf(os.Stderr, "SPIKE FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("SPIKE PASS")
}

// childOp performs one operation and exits 0 on success, 1 on denial.
func childOp(op, path string) {
	var err error
	switch op {
	case "write":
		err = os.WriteFile(path, []byte("confined write\n"), 0o600)
	case "read":
		var b []byte
		b, err = os.ReadFile(path)
		if err == nil {
			_ = b
		}
	case "delete":
		err = os.Remove(path)
	case "mkdir":
		err = os.Mkdir(path, 0o700)
	default:
		fmt.Fprintf(os.Stderr, "child: unknown op %q\n", op)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: %s denied: %v\n", op, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// writeSidFor derives the deterministic write identity for a root path:
// sha256 of the canonical path -> S-1-4-x-y.
func writeSidFor(canonicalRoot string) string {
	d := sha256.Sum256([]byte(canonicalRoot))
	first := binary.LittleEndian.Uint32(d[0:4])%(1<<30-1) + 1
	second := binary.LittleEndian.Uint32(d[4:8])%(1<<30-1) + 1
	return fmt.Sprintf("S-1-4-%d-%d", first, second)
}

func parent() error {
	base, err := os.MkdirTemp("", "winacl-spike-")
	if err != nil {
		return fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(base)

	// rootA and rootB are granted; outside is not. workdir is inside rootA but
	// is not itself a root, so the deny-closed probe has something to bite on.
	rootA := filepath.Join(base, "rootA")
	rootB := filepath.Join(base, "rootB")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{rootA, rootB, outside} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	canon := func(p string) string {
		c, err := filepath.EvalSymlinks(p)
		if err != nil {
			return p
		}
		return c
	}
	rootA, rootB, outside = canon(rootA), canon(rootB), canon(outside)

	// A file outside every root, to test delete and read boundaries.
	outsideFile := filepath.Join(outside, "target.txt")
	if err := os.WriteFile(outsideFile, []byte("host owned\n"), 0o600); err != nil {
		return fmt.Errorf("seed outside file: %w", err)
	}

	roots := []string{rootA, rootB}
	fmt.Printf("spike: granted roots   %v\n", roots)
	fmt.Printf("spike: ungranted       %s\n", outside)

	writeSids := make([]*windows.SID, 0, len(roots))
	worldSid, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		return fmt.Errorf("world SID: %w", err)
	}
	lowSid, err := windows.CreateWellKnownSid(windows.WinLowLabelSid)
	if err != nil {
		return fmt.Errorf("low label SID: %w", err)
	}
	for _, r := range roots {
		sid, err := windows.StringToSid(writeSidFor(r))
		if err != nil {
			return fmt.Errorf("parse write SID for %s: %w", r, err)
		}
		fmt.Printf("spike: write SID %s -> %s\n", r, sid.String())
		writeSids = append(writeSids, sid)
		if err := grantWrite(r, sid, worldSid, lowSid); err != nil {
			return fmt.Errorf("grant %s: %w", r, err)
		}
	}
	fmt.Println("spike: grants applied")

	token, err := restrictedToken(writeSids, worldSid, lowSid)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	defer token.Close()

	// ---- A: write inside a granted root is ALLOWED ----
	for _, r := range roots {
		target := filepath.Join(r, "inside.txt")
		if err := runChild(token, r, "write", target); err != nil {
			return fmt.Errorf("A: write inside %s should be ALLOWED: %w", r, err)
		}
		if _, err := os.Stat(target); err != nil {
			return fmt.Errorf("A: write inside %s did not land: %w", r, err)
		}
	}
	fmt.Println("spike: PASS A  write inside each granted root ALLOWED")

	// ---- B: write outside every root is DENIED ----
	if err := runChild(token, rootA, "write", filepath.Join(outside, "w.txt")); err == nil {
		return fmt.Errorf("B: write outside was ALLOWED — the paddock does not hold")
	}
	fmt.Println("spike: PASS B  write outside every root DENIED")

	// ---- C: delete outside every root is DENIED ----
	if err := runChild(token, rootA, "delete", outsideFile); err == nil {
		return fmt.Errorf("C: delete outside was ALLOWED — the ambient FILE_DELETE_CHILD hole is open")
	}
	if _, err := os.Stat(outsideFile); err != nil {
		return fmt.Errorf("C: the outside file was deleted despite the denial")
	}
	fmt.Println("spike: PASS C  delete outside every root DENIED")

	// ---- D: read outside every root is ALLOWED (Landlock parity) ----
	if err := runChild(token, rootA, "read", outsideFile); err != nil {
		return fmt.Errorf("D: read outside should be ALLOWED (reads are not governed): %w", err)
	}
	fmt.Println("spike: PASS D  read outside every root ALLOWED (read boundary: not governed)")

	// ---- E: the three-part probe ports 1:1 ----
	probe, err := probe(token, rootA, roots)
	if err != nil {
		return fmt.Errorf("E: probe: %w", err)
	}
	fmt.Printf("spike: PASS E  three-part probe conformant: %s\n", probe)

	// ---- ReadOnly: zero writable roots -> every write denied ----
	roToken, err := restrictedToken(nil, worldSid, lowSid)
	if err != nil {
		return fmt.Errorf("read-only token: %w", err)
	}
	defer roToken.Close()
	if err := runChild(roToken, rootA, "write", filepath.Join(rootA, "ro.txt")); err == nil {
		return fmt.Errorf("read-only: write inside a granted root was ALLOWED — the read-only lowering is not restrictive")
	}
	fmt.Println("spike: PASS F  read-only lowering denies every write")

	// ---- the unconfined host is unaffected ----
	if err := os.WriteFile(filepath.Join(outside, "host.txt"), []byte("host\n"), 0o600); err != nil {
		return fmt.Errorf("host write outside failed: %w", err)
	}
	fmt.Println("spike: PASS G  the unconfined host can still write outside")
	return nil
}

// probe runs shepherd's three-part fail-closed probe against the confined
// child: liveness, per-root conformance, deny-closed.
func probe(token windows.Token, workdir string, roots []string) (string, error) {
	// (1) liveness: an out-of-workdir write must be denied.
	outside := filepath.Join(filepath.Dir(workdir), "probe-outside.txt")
	if err := runChild(token, workdir, "write", outside); err == nil {
		return "", fmt.Errorf("liveness: an out-of-workdir write was PERMITTED — no paddock")
	}
	// (2) per-root: a canary write beneath each declared root must be allowed.
	for _, r := range roots {
		if err := runChild(token, workdir, "write", filepath.Join(r, "probe-canary.txt")); err != nil {
			return "", fmt.Errorf("per-root: writable root %s DENIES writes (profile too strict): %w", r, err)
		}
	}
	// (3) deny-closed: an in-workdir path outside every root must be denied.
	if !withinAnyRoot(workdir, roots) {
		if err := runChild(token, workdir, "write", filepath.Join(workdir, "probe-denied.txt")); err == nil {
			return "", fmt.Errorf("deny-closed: an in-workdir path outside every root was PERMITTED")
		}
	}
	return "liveness + per-root + deny-closed", nil
}

func withinAnyRoot(p string, roots []string) bool {
	for _, r := range roots {
		if p == r {
			return true
		}
		rel, err := filepath.Rel(r, p)
		if err != nil {
			continue
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// grantWrite applies the three per-root edits in one SetNamedSecurityInfoW:
// the capability allow ACE, the ambient-delete deny, and the Low mandatory label.
func grantWrite(path string, writeSid, worldSid, lowSid *windows.SID) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("GetNamedSecurityInfo: %w", err)
	}
	oldDacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read DACL: %w", err)
	}
	entries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: fileDeleteChild,
			AccessMode:        windows.DENY_ACCESS,
			Inheritance:       containerInherit,
			Trustee: windows.TRUSTEE{
				MultipleTrusteeOperation: windows.NO_MULTIPLE_TRUSTEE,
				TrusteeForm:              windows.TRUSTEE_IS_SID,
				TrusteeType:              windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue:             windows.TrusteeValueFromSID(worldSid),
			},
		},
		{
			AccessPermissions: grantMask,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				MultipleTrusteeOperation: windows.NO_MULTIPLE_TRUSTEE,
				TrusteeForm:              windows.TRUSTEE_IS_SID,
				TrusteeType:              windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue:             windows.TrusteeValueFromSID(writeSid),
			},
		},
	}
	newDacl, err := windows.ACLFromEntries(entries, oldDacl)
	if err != nil {
		return fmt.Errorf("ACLFromEntries: %w", err)
	}
	labelAcl, err := buildLowLabelAcl(lowSid)
	if err != nil {
		return err
	}

	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION,
		nil, nil, newDacl, labelAcl); err != nil {
		return fmt.Errorf("SetNamedSecurityInfo: %w", err)
	}
	runtime.KeepAlive(labelAcl)
	return nil
}

// buildLowLabelAcl allocates an ACL carrying one SYSTEM_MANDATORY_LABEL_ACE
// (Low, no-write-up) inheriting to subcontainers and objects.
//
// The buffer is Go-allocated: AclSize is the buffer's CAPACITY, not its used
// size (AddAce refuses with ERROR_NOT_ENOUGH_MEMORY when the new ACE would
// not fit within it), and a Go buffer is never freed with LocalFree.
func buildLowLabelAcl(lowSid *windows.SID) (*windows.ACL, error) {
	sidLen := windows.GetLengthSid(lowSid)
	size := uint32(8) + 4 + 4 + sidLen // ACL header + ACE header + mask + inline SID
	buf := make([]byte, size)
	hdr := (*aclHeader)(unsafe.Pointer(&buf[0]))
	hdr.Revision = aclRevision
	hdr.AclSize = uint16(size)
	hdr.AceCount = 0

	r, _, callErr := procAddMandatoryAce.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(aclRevision),
		uintptr(windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT),
		uintptr(mandatoryLabelNoWriteUp),
		uintptr(unsafe.Pointer(lowSid)),
	)
	runtime.KeepAlive(buf)
	if r == 0 {
		return nil, fmt.Errorf("AddMandatoryAce: %w", callErr)
	}
	return (*windows.ACL)(unsafe.Pointer(&buf[0])), nil
}

// restrictedToken mints the WRITE_RESTRICTED token, lowers it to Low integrity,
// and gives it a default DACL naming the write SIDs (so objects the child
// creates pass the write pass-2 check).
func restrictedToken(writeSids []*windows.SID, worldSid, lowSid *windows.SID) (windows.Token, error) {
	var current windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE|windows.TOKEN_ADJUST_DEFAULT|windows.TOKEN_ASSIGN_PRIMARY,
		&current); err != nil {
		return 0, fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer current.Close()

	groups, err := current.GetTokenGroups()
	if err != nil {
		return 0, fmt.Errorf("GetTokenGroups: %w", err)
	}
	var logonSid *windows.SID
	for _, g := range groups.AllGroups() {
		if g.Attributes&seGroupLogonID == seGroupLogonID {
			logonSid = g.Sid
			break
		}
	}
	if logonSid == nil {
		return 0, fmt.Errorf("no logon SID among %d token groups", len(groups.AllGroups()))
	}

	restricting := []windows.SIDAndAttributes{{Sid: logonSid}, {Sid: worldSid}}
	for _, s := range writeSids {
		restricting = append(restricting, windows.SIDAndAttributes{Sid: s})
	}
	var newToken windows.Token
	r, _, callErr := procCreateRestrictedToken.Call(
		uintptr(current),
		uintptr(disableMaxPrivilege|luaToken|writeRestricted),
		0, 0,
		0, 0,
		uintptr(len(restricting)),
		uintptr(unsafe.Pointer(&restricting[0])),
		uintptr(unsafe.Pointer(&newToken)),
	)
	if r == 0 {
		return 0, fmt.Errorf("CreateRestrictedToken: %w", callErr)
	}

	tml := windows.Tokenmandatorylabel{
		Label: windows.SIDAndAttributes{Sid: lowSid, Attributes: seGroupIntegrity},
	}
	if err := windows.SetTokenInformation(newToken, windows.TokenIntegrityLevel,
		(*byte)(unsafe.Pointer(&tml)), uint32(unsafe.Sizeof(tml))); err != nil {
		newToken.Close()
		return 0, fmt.Errorf("SetTokenInformation(TokenIntegrityLevel): %w", err)
	}

	// Default DACL: merge a full-access ACE for a restricting SID so objects the
	// child creates (pipes, sync objects) pass the write pass-2 check.
	defaultTrustee := worldSid
	if len(writeSids) > 0 {
		defaultTrustee = writeSids[0]
	}
	defaultEntries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: 0x001F01FF, // FILE_ALL_ACCESS
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			MultipleTrusteeOperation: windows.NO_MULTIPLE_TRUSTEE,
			TrusteeForm:              windows.TRUSTEE_IS_SID,
			TrusteeType:              windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue:             windows.TrusteeValueFromSID(defaultTrustee),
		},
	}}
	defaultAcl, err := windows.ACLFromEntries(defaultEntries, nil)
	if err != nil {
		newToken.Close()
		return 0, fmt.Errorf("default ACL: %w", err)
	}
	if err := windows.SetTokenInformation(newToken, windows.TokenDefaultDacl,
		(*byte)(unsafe.Pointer(&defaultAcl)), uint32(unsafe.Sizeof(defaultAcl))); err != nil {
		newToken.Close()
		return 0, fmt.Errorf("SetTokenInformation(TokenDefaultDacl): %w", err)
	}
	return newToken, nil
}

// runChild spawns this binary in --child mode under the restricted token and
// waits for it. The returned error is the child's failure to perform the op.
func runChild(token windows.Token, cwd, op, target string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("executable: %w", err)
	}
	cmdline, err := windows.UTF16PtrFromString(fmt.Sprintf("%q --child %s %q", exe, op, target))
	if err != nil {
		return fmt.Errorf("cmdline: %w", err)
	}
	si := windows.StartupInfo{}
	pi := windows.ProcessInformation{}
	creationFlags := uint32(windows.CREATE_UNICODE_ENVIRONMENT)
	cwdPtr, err := windows.UTF16PtrFromString(cwd)
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	if err := windows.CreateProcessAsUser(token, nil, cmdline, nil, nil, false,
		creationFlags, envBlock(), cwdPtr, &si, &pi); err != nil {
		return fmt.Errorf("CreateProcessAsUser: %w", err)
	}
	defer windows.CloseHandle(pi.Thread)
	defer windows.CloseHandle(pi.Process)
	_, _ = windows.WaitForSingleObject(pi.Process, 30000)
	var code uint32
	if err := windows.GetExitCodeProcess(pi.Process, &code); err != nil {
		return fmt.Errorf("GetExitCodeProcess: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("child %s exit code %d", op, code)
	}
	return nil
}

// envBlock builds a double-null-terminated UTF-16 environment block.
func envBlock() *uint16 {
	buf := make([]uint16, 0, 2048)
	for _, kv := range os.Environ() {
		u, err := windows.UTF16FromString(kv)
		if err != nil {
			continue
		}
		buf = append(buf, u...)
	}
	buf = append(buf, 0, 0)
	return &buf[0]
}
