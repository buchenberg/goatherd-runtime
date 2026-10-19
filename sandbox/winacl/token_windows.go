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
//
// The values are the SDK's. LUA_TOKEN is 0x4 — 0x2 is SANDBOX_INERT, which
// is the opposite of a restriction: it tells the system not to apply
// AppLocker rules or software restriction policies to the token. Asking for
// it by mistake would exempt the confined child from the host's application
// control policy.
const (
	disableMaxPrivilege = 0x00000001
	sandboxInert        = 0x00000002
	luaToken            = 0x00000004
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
	return createRestrictedTokenWith(tok, restricting)
}

// createRestrictedTokenWith mints the write-restricted token from an explicit
// restricting-SID list, optionally marking SIDs deny-only.
func createRestrictedTokenWith(tok windows.Token, restricting []windows.SIDAndAttributes) (windows.Token, error) {
	return createRestrictedTokenFull(tok, nil, restricting)
}

// createRestrictedTokenFull mints the write-restricted token from explicit
// deny-only and restricting SID lists.
func createRestrictedTokenFull(tok windows.Token, disable, restricting []windows.SIDAndAttributes) (windows.Token, error) {
	var restricted windows.Token
	var sidPtr uintptr
	if len(restricting) > 0 {
		sidPtr = uintptr(unsafe.Pointer(&restricting[0]))
	}
	var disablePtr uintptr
	if len(disable) > 0 {
		disablePtr = uintptr(unsafe.Pointer(&disable[0]))
	}
	rc, _, callErr := procCreateRestrictedToken.Call(
		uintptr(tok),
		uintptr(disableMaxPrivilege|luaToken|writeRestricted),
		uintptr(len(disable)), disablePtr,
		0, 0, // no privileges deleted
		uintptr(len(restricting)),
		sidPtr,
		uintptr(unsafe.Pointer(&restricted)),
	)
	runtime.KeepAlive(restricting)
	runtime.KeepAlive(disable)
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
// The merge is into the token's EXISTING default DACL, not a replacement: a
// replacement would name only the capability SID, which is in the restricting
// list but not in the token's normal SID set — so the child could create an
// object and then fail the *normal* access check when it reopened it.
//
// The merged ACE names the workspace write SID when there is one, else
// Everyone (read-only mode). Object creation stays gated by the parent
// object's DACL; only the new object's own DACL passes the second check.
func setDefaultDacl(tok windows.Token, sid *windows.SID) error {
	existing, err := tokenDefaultDacl(tok)
	if err != nil {
		return fmt.Errorf("read the token default DACL: %w", err)
	}
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
	acl, err := windows.ACLFromEntries(entries, existing)
	if err != nil {
		return fmt.Errorf("merge into the token default DACL: %w", err)
	}
	if err := windows.SetTokenInformation(tok, windows.TokenDefaultDacl,
		(*byte)(unsafe.Pointer(&acl)), uint32(unsafe.Sizeof(acl))); err != nil {
		return fmt.Errorf("set the token default DACL: %w", err)
	}
	runtime.KeepAlive(acl)
	return nil
}

// tokenDefaultDacl returns the token's current default DACL, or nil when it
// carries none.
func tokenDefaultDacl(tok windows.Token) (*windows.ACL, error) {
	var needed uint32
	if err := windows.GetTokenInformation(tok, windows.TokenDefaultDacl, nil, 0, &needed); err != nil {
		// The size query is expected to fail with ERROR_INSUFFICIENT_BUFFER.
		return nil, nil
	}
	if needed == 0 {
		return nil, nil
	}
	buffer := make([]byte, needed)
	if err := windows.GetTokenInformation(tok, windows.TokenDefaultDacl,
		(*byte)(unsafe.Pointer(&buffer[0])), uint32(len(buffer)), &needed); err != nil {
		return nil, err
	}
	// TOKEN_DEFAULT_DACL is a single PACL; the buffer's first pointer is it.
	return (*windows.ACL)(unsafe.Pointer(&buffer[0])), nil
}
