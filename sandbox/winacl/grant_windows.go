//go:build windows

package winacl

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// lockFilePath returns the path of the cross-process lock that serializes
// the security-descriptor transactions on path.
//
// The lock lives under the system temp directory, never under the guarded
// path (a lock inside the tree would inherit the grant and be writable by the
// confined child) and never in the runner's argv. The lowercased path hashes
// to the name so Windows's case-insensitive spellings map onto one lock.
func lockFilePath(path string) (string, error) {
	temp := os.TempDir()
	if temp == "" {
		return "", fmt.Errorf("winacl: no temp directory for the grant lock")
	}
	digest := sha256.Sum256([]byte(strings.ToLower(path)))
	return filepath.Join(temp, "goatherd-winacl-locks", fmt.Sprintf("%x.lock", digest[:8])), nil
}

// withPathLock runs action holding the exclusive lock for path.
//
// The whole get-merge-set sequence runs under it: without it, two concurrent
// grants read the same descriptor and the second write discards the first's
// ACE — and a grant racing a revoke can restore a capability that was just
// removed.
//
// The wait is BOUNDED. The lock file's path is predictable and any
// same-user process can hold the lock, so an unbounded wait would let a
// process that holds it stall every confined launch on this host. The lock
// is therefore taken with LOCKFILE_FAIL_IMMEDIATELY and retried until a
// deadline, after which the transaction refuses rather than hanging.
func withPathLock(path string, action func() error) error {
	lockPath, err := lockFilePath(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return fmt.Errorf("winacl: create the grant lock directory: %w", err)
	}
	// The lock file is opened WITHOUT delete sharing: a lock that could be
	// removed and recreated under the holder would let two processes hold it.
	lockPathUTF16, err := windows.UTF16PtrFromString(lockPath)
	if err != nil {
		return fmt.Errorf("winacl: encode the grant lock path: %w", err)
	}
	handle, err := windows.CreateFile(lockPathUTF16,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return fmt.Errorf("winacl: open the grant lock: %w", err)
	}
	defer windows.CloseHandle(handle)

	overlapped := new(windows.Overlapped)
	deadline := time.Now().Add(lockTimeout)
	for {
		err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
		if err == nil {
			break
		}
		// Only a held lock is worth waiting for. Any other failure is
		// permanent — no byte-range lock support, an invalid handle — and
		// retrying it for the whole deadline would stall every grant on the
		// host and misreport the cause.
		if err != windows.ERROR_LOCK_VIOLATION {
			return fmt.Errorf("winacl: lock %s: %w", lockPath, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("winacl: the grant lock on %s is held by another process (%w)", path, err)
		}
		time.Sleep(lockRetryInterval)
	}
	defer func() {
		_ = windows.UnlockFileEx(handle, 0, 1, 0, overlapped)
	}()
	return action()
}

// The grant lock's wait bounds.
const (
	lockTimeout       = 30 * time.Second
	lockRetryInterval = 50 * time.Millisecond
)

// GrantWrite applies the workspace grant to path: the write capability ACE,
// the ambient-delete deny, and the Low no-write-up mandatory label.
//
// The three edits are applied in one SetNamedSecurityInfo call so the DACL
// and the label move together — the write is admitted only where the
// capability ACE allows it AND the Low label clears the integrity check, and
// a half-applied grant would be a grant that does not mean what it says.
//
// The grant is idempotent: when the exact ACE, the exact deny, and the exact
// label already stand, the apply is skipped. That matters because the apply
// is an eager, tree-wide security descriptor write — NTFS propagates an
// inheritable ACE to every descendant at once — so repeating it would cost
// the same propagation on every provision.
//
// The path must be owned by the caller and grant WRITE_OWNER: the mandatory
// label lives in the SACL, and the owner's implicit rights cover only
// READ_CONTROL and WRITE_DAC. A directory that grants only Modify therefore
// fails loudly here rather than silently losing the confinement.
func GrantWrite(path string, writeSID, worldSID, lowSID *windows.SID) error {
	return withPathLock(path, func() error { return grantWrite(path, writeSID, worldSID, lowSID) })
}

// grantWrite is the transaction body of GrantWrite, under the path lock.
func grantWrite(path string, writeSID, worldSID, lowSID *windows.SID) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read the security descriptor of %s: %w", path, err)
	}
	// windows.GetNamedSecurityInfo already frees the Win32 descriptor and
	// returns a Go-allocated copy of it, so there is nothing to free here —
	// freeing it again would corrupt the heap. The DACL and SACL pointers
	// below point into that copy.

	currentDacl, _, daclErr := sd.DACL()
	if daclErr != nil || currentDacl == nil {
		return fmt.Errorf(
			"the DACL of %s is absent or null; this mechanism confines by merging into an existing DACL, "+
				"so the directory needs a real one before it can be confined", path)
	}
	daclBytes := aclView(currentDacl)
	labelBytes := labelACLBytes(sd)

	// The exact-ACE skip: the grant, the deny, and the label all stand.
	if hasExactACE(daclBytes, accessAllowedAceType, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT, grantMask, writeSID) &&
		hasExactACE(daclBytes, accessDeniedAceType, containerInherit, fileDeleteChild, worldSID) &&
		hasExactACE(labelBytes, systemMandatoryLabelAceType, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT, mandatoryLabelNoWriteUp, lowSID) {
		return nil
	}

	entries := []windows.EXPLICIT_ACCESS{
		{
			// The ambient-delete deny. FILE_DELETE_CHILD is evaluated on
			// directories, so the deny inherits to containers only: its bit
			// (0x40) is a member of FILE_ALL_ACCESS, and inheriting it onto
			// files would refuse every GENERIC_ALL open inside the root.
			AccessPermissions: fileDeleteChild,
			AccessMode:        windows.DENY_ACCESS,
			Inheritance:       containerInherit,
			Trustee: windows.TRUSTEE{
				MultipleTrusteeOperation: windows.NO_MULTIPLE_TRUSTEE,
				TrusteeForm:              windows.TRUSTEE_IS_SID,
				TrusteeType:              windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue:             windows.TrusteeValueFromSID(worldSID),
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
				TrusteeValue:             windows.TrusteeValueFromSID(writeSID),
			},
		},
	}
	newDacl, err := windows.ACLFromEntries(entries, currentDacl)
	if err != nil {
		return fmt.Errorf("merge the write grant into the DACL of %s: %w", path, err)
	}
	newLabel, err := buildLabelACL(labelBytes, lowSID)
	if err != nil {
		return fmt.Errorf("build the mandatory label for %s: %w", path, err)
	}
	labelAcl := (*windows.ACL)(unsafe.Pointer(&newLabel[0]))

	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION,
		nil, nil, newDacl, labelAcl); err != nil {
		return fmt.Errorf("apply the write grant to %s: %w", path, err)
	}
	runtime.KeepAlive(newLabel)
	return nil
}

// RevokeWrite removes the write capability ACE from a path's DACL.
//
// The ambient-delete deny and the Low label deliberately remain: the deny
// narrows authority, and the label is the price of the write boundary —
// removing it could hand the write back to a process still holding the
// capability. A directory whose only remaining grant was this one keeps a
// Low label, which is a standing change to a real directory and is recorded
// as such.
func RevokeWrite(path string, writeSID *windows.SID) error {
	return withPathLock(path, func() error { return revokeWrite(path, writeSID) })
}

// revokeWrite is the transaction body of RevokeWrite, under the path lock.
func revokeWrite(path string, writeSID *windows.SID) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read the security descriptor of %s: %w", path, err)
	}
	// The descriptor is a Go-allocated copy (see GrantWrite).

	currentDacl, _, daclErr := sd.DACL()
	if daclErr != nil || currentDacl == nil {
		return nil
	}
	entries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: 0, // REVOKE_ACCESS removes every ACE for the trustee.
		AccessMode:        windows.REVOKE_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			MultipleTrusteeOperation: windows.NO_MULTIPLE_TRUSTEE,
			TrusteeForm:              windows.TRUSTEE_IS_SID,
			TrusteeType:              windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue:             windows.TrusteeValueFromSID(writeSID),
		},
	}}
	newDacl, err := windows.ACLFromEntries(entries, currentDacl)
	if err != nil {
		return fmt.Errorf("merge the revoke into the DACL of %s: %w", path, err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION, nil, nil, newDacl, nil); err != nil {
		return fmt.Errorf("apply the revoke to %s: %w", path, err)
	}
	return nil
}
