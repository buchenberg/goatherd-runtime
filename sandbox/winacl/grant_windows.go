//go:build windows

package winacl

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
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
