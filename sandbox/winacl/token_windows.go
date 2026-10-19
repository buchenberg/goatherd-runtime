//go:build windows

package winacl

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	advapi32                  = windows.NewLazySystemDLL("advapi32.dll")
	procCreateRestrictedToken = advapi32.NewProc("CreateRestrictedToken")
)

// CreateRestrictedToken flags.
const (
	disableMaxPrivilege = 0x00000001
	luaToken            = 0x00000002
	writeRestricted     = 0x00000008
)

// Token group attributes.
const (
	seGroupLogonID   = 0xC0000000
	seGroupIntegrity = 0x00000020
)

// fileAllAccess is FILE_ALL_ACCESS — the mask a new object created by the
// confined child takes from the token's default DACL.
const fileAllAccess = 0x001F01FF

// tokenAccess is the access the process token needs.
//
// CreateRestrictedToken requires TOKEN_DUPLICATE on the existing token handle.
// windows.OpenCurrentProcessToken opens with TOKEN_QUERY only, so the token is
// opened here explicitly: without TOKEN_DUPLICATE the call fails with
// ERROR_ACCESS_DENIED even when no flags and no restricting SIDs are passed —
// which reads like a privilege wall and is not one.
const tokenAccess = windows.TOKEN_QUERY | windows.TOKEN_DUPLICATE |
	windows.TOKEN_ADJUST_DEFAULT | windows.TOKEN_ASSIGN_PRIMARY

// openProcessToken opens the current process's token with the rights the
// restriction needs.
func openProcessToken() (windows.Token, error) {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), tokenAccess, &tok); err != nil {
		return 0, fmt.Errorf("open the process token: %w", err)
	}
	return tok, nil
}

// logonSID returns the token's logon session SID (S-1-5-5-x-y, attribute
// SE_GROUP_LOGON_ID).
//
// The restricted token needs it: the per-logon objects a child touches (the
// window station, the desktop) are keyed by it, and a restricted token without
// it cannot initialize. The SID is copied out of the token's group buffer so
// it does not dangle once that buffer is collected.
func logonSID(tok windows.Token) (*windows.SID, error) {
	groups, err := tok.GetTokenGroups()
	if err != nil {
		return nil, fmt.Errorf("read the token groups: %w", err)
	}
	all := groups.AllGroups()
	for i := range all {
		if all[i].Attributes&seGroupLogonID != seGroupLogonID {
			continue
		}
		copied, err := all[i].Sid.Copy()
		if err != nil {
			return nil, fmt.Errorf("copy the logon SID: %w", err)
		}
		return copied, nil
	}
	return nil, fmt.Errorf("no logon SID among the token's %d groups", len(all))
}

// createRestrictedToken mints the write-restricted token.
//
// The restricting SIDs are the logon session SID, Everyone, and one write
// capability SID per granted root. The access check then runs twice — once
// against the token's normal SIDs, once against the restricting SIDs — and a
// write is granted only where both allow it.
//
// Everyone is in the list for a reason that is not about write authority:
// without the logon SID, early DLL initialization dies with 0xC0000142, and
// without Everyone, CNG (\Device\CNG) fails and pwsh crashes with
// 0xE0434352. Everyone confers no write authority on its own — the Low
// integrity label on the granted roots denies that (see GrantWrite).
//
// A restricted token is a restricted version of the caller's own primary
// token, so CreateProcessAsUser needs no SE_ASSIGNPRIMARYTOKEN_NAME privilege
// for it; the restriction is available to an ordinary, unelevated process.
func createRestrictedToken(tok windows.Token, writeSIDs []*windows.SID, worldSID *windows.SID) (windows.Token, error) {
	logon, err := logonSID(tok)
	if err != nil {
		return 0, err
	}
	restricting := make([]windows.SIDAndAttributes, 0, 2+len(writeSIDs))
	restricting = append(restricting,
		windows.SIDAndAttributes{Sid: logon},
		windows.SIDAndAttributes{Sid: worldSID},
	)
	for _, sid := range writeSIDs {
		restricting = append(restricting, windows.SIDAndAttributes{Sid: sid})
	}
	var restricted windows.Token
	rc, _, callErr := procCreateRestrictedToken.Call(
		uintptr(tok),
		uintptr(disableMaxPrivilege|luaToken|writeRestricted),
		0, 0, // no SIDs disabled
		0, 0, // no privileges deleted
		uintptr(len(restricting)),
		uintptr(unsafe.Pointer(&restricting[0])),
		uintptr(unsafe.Pointer(&restricted)),
	)
	runtime.KeepAlive(restricting)
	if rc == 0 {
		return 0, fmt.Errorf("CreateRestrictedToken: %w", callErr)
	}
	return restricted, nil
}

// lowerIntegrity sets the token's integrity level to Low (S-1-16-4096).
//
// The level is what the mandatory labels on the granted roots are matched
// against: a token left at Medium ignores them, and the write to an ungranted
// directory would be admitted by the write pass-2 check alone.
func lowerIntegrity(tok windows.Token, lowSID *windows.SID) error {
	label := windows.Tokenmandatorylabel{
		Label: windows.SIDAndAttributes{Sid: lowSID, Attributes: seGroupIntegrity},
	}
	if err := windows.SetTokenInformation(tok, windows.TokenIntegrityLevel,
		(*byte)(unsafe.Pointer(&label)), uint32(unsafe.Sizeof(label))); err != nil {
		return fmt.Errorf("set the token integrity level: %w", err)
	}
	return nil
}

// setDefaultDacl gives the token a default DACL that names a restricting SID.
//
// A restricted token inherits the user's default DACL, which names no
// restricting SID. Every object the confined child then creates (an anonymous
// stdio pipe, a sync object) takes that default, so the write pass-2 check
// denies its creation and every piped-stdio grandchild spawn fails with
// ERROR_ACCESS_DENIED.
//
// The merged ACE names the private temp write SID when one exists, else the
// workspace write SID, else Everyone (read-only mode). Object creation stays
// gated by the parent object's DACL; only the new object's own DACL passes
// the second check.
func setDefaultDacl(tok windows.Token, sid *windows.SID) error {
	entries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: fileAllAccess,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			MultipleTrusteeOperation: windows.NO_MULTIPLE_TRUSTEE,
			TrusteeForm:              windows.TRUSTEE_IS_SID,
			TrusteeType:              windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue:             windows.TrusteeValueFromSID(sid),
		},
	}}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("build the token default DACL: %w", err)
	}
	if err := windows.SetTokenInformation(tok, windows.TokenDefaultDacl,
		(*byte)(unsafe.Pointer(&acl)), uint32(unsafe.Sizeof(acl))); err != nil {
		return fmt.Errorf("set the token default DACL: %w", err)
	}
	runtime.KeepAlive(acl)
	return nil
}
