//go:build windows

package winacl

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procAddMandatoryAce appends a mandatory label ACE to an ACL.
//
// golang.org/x/sys/windows does not wrap AddMandatoryAce (nor InitializeAcl
// nor AddAce), and the ACL struct it exposes has unexported fields, so the
// ACL header is written by hand and the ACE is appended through the Win32
// entry point.
var procAddMandatoryAce = windows.NewLazySystemDLL("advapi32.dll").NewProc("AddMandatoryAce")

// ACE types and masks the grant reads and writes.
const (
	accessAllowedAceType        = 0  // ACCESS_ALLOWED_ACE_TYPE
	accessDeniedAceType         = 1  // ACCESS_DENIED_ACE_TYPE
	systemMandatoryLabelAceType = 17 // SYSTEM_MANDATORY_LABEL_ACE_TYPE

	// aclRevision is ACL_REVISION.
	aclRevision = 2
	// mandatoryLabelNoWriteUp is SYSTEM_MANDATORY_LABEL_NO_WRITE_UP.
	mandatoryLabelNoWriteUp = 0x1
	// containerInherit is CONTAINER_INHERIT_ACE.
	containerInherit = 0x00000002
	// fileDeleteChild is FILE_DELETE_CHILD.
	fileDeleteChild = 0x00000040
)

// The file access rights the capability grants. The values are the SDK's
// FILE_* constants; they are spelled out rather than folded into a hex
// literal because the two that are easy to lose are the two that break a
// build: FILE_APPEND_DATA (FILE_ADD_SUBDIRECTORY on a directory) and
// FILE_WRITE_ATTRIBUTES. A mask without them refuses a directory creation
// inside the granted root.
const (
	fileReadData        = 0x00000001
	fileWriteData       = 0x00000002
	fileAppendData      = 0x00000004
	fileReadEA          = 0x00000008
	fileWriteEA         = 0x00000010
	fileExecute         = 0x00000020
	fileReadAttributes  = 0x00000080
	fileWriteAttributes = 0x00000100
	fileDelete          = 0x00010000
	fileReadControl     = 0x00020000
	fileSynchronize     = 0x00100000
)

// grantMask is the capability's allowlist: the write-class rights plus the
// delete, read-control, and synchronize rights a body needs to work with the
// files it creates.
//
// The write-class half is what the write-restricted intersection consults.
// FILE_APPEND_DATA (FILE_ADD_SUBDIRECTORY on a directory) and
// FILE_WRITE_ATTRIBUTES are part of it: without them a confined body cannot
// create a directory or set an attribute inside a granted root, which is a
// routine thing for a build to do.
const grantMask = fileWriteData | fileAppendData | fileWriteEA | fileWriteAttributes |
	fileReadAttributes | fileDelete | fileReadControl | fileSynchronize

// aclHeader is the documented on-disk ACL header (8 bytes). The ACL struct
// x/sys exposes has unexported fields, so the header is read through a byte
// view instead.
type aclHeader struct {
	Revision byte
	Sbz1     byte
	AclSize  uint16
	AceCount uint16
	Sbz2     uint16
}

// readUint16 reads a little-endian uint16 from a byte view.
func readUint16(b []byte, off int) uint16 {
	return uint16(b[off]) | uint16(b[off+1])<<8
}

// readUint32 reads a little-endian uint32 from a byte view.
func readUint32(b []byte, off int) uint32 {
	return uint32(b[off]) | uint32(b[off+1])<<8 | uint32(b[off+2])<<16 | uint32(b[off+3])<<24
}

// aclView returns a read-only byte view over an ACL.
//
// The view is bounded by the ACL's own AclSize field, so a malformed length
// cannot walk past the allocation. A nil or implausible ACL yields no view,
// which the callers treat as "no matching entry" — the safe direction, since
// it makes the grant apply rather than skip.
func aclView(acl *windows.ACL) []byte {
	if acl == nil {
		return nil
	}
	size := int(readUint16(unsafe.Slice((*byte)(unsafe.Pointer(acl)), 8), 2))
	if size < 8 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(acl)), size)
}

// forEachACE walks an ACL's ACEs.
//
// The callback receives the ACE type, its flags, its access mask, and the
// offset of the ACE within the view; the inline SID of a standard ACE follows
// the 4-byte header and the 4-byte mask, at aceStart+8. Returning false stops
// the walk.
//
// A malformed ACE (an implausible size, or one running past the view) ends
// the walk: a truncated ACE list is not evidence of an exact entry.
func forEachACE(view []byte, fn func(aceType, aceFlags byte, mask uint32, aceStart int) bool) {
	if len(view) < 8 {
		return
	}
	count := int(readUint16(view, 4))
	off := 8
	for i := 0; i < count; i++ {
		if off+8 > len(view) {
			return
		}
		size := int(readUint16(view, off+2))
		if size < 8 || off+size > len(view) {
			return
		}
		if !fn(view[off], view[off+1], readUint32(view, off+4), off) {
			return
		}
		off += size
	}
}

// inlineSIDEquals reports whether the inline SID of an ACE is the given SID.
//
// The SID is inline in the ACE (there is no pointer to follow), so it is read
// as bytes and compared with the Win32 comparison.
func inlineSIDEquals(view []byte, aceStart int, want *windows.SID) bool {
	off := aceStart + 8
	if off+2 > len(view) {
		return false
	}
	sidLen := 2 + 6 + int(view[off+1])*4
	if off+sidLen > len(view) {
		return false
	}
	inline := (*windows.SID)(unsafe.Pointer(&view[off]))
	return windows.EqualSid(inline, want)
}

// hasExactACE reports whether an ACL carries exactly the given entry: the ACE
// type, the flags, the access mask, and the trustee SID. An ACE that matches
// on the first three but names a different SID is not the entry.
func hasExactACE(view []byte, aceType, aceFlags byte, mask uint32, want *windows.SID) bool {
	found := false
	forEachACE(view, func(t, f byte, m uint32, aceStart int) bool {
		if t == aceType && f == aceFlags && m == mask && inlineSIDEquals(view, aceStart, want) {
			found = true
			return false
		}
		return true
	})
	return found
}

// labelACLBytes returns a byte view of a directory's mandatory-label ACL, or
// nil when the directory carries no label.
func labelACLBytes(sd *windows.SECURITY_DESCRIPTOR) []byte {
	sacl, _, err := sd.SACL()
	if err != nil || sacl == nil {
		return nil
	}
	return aclView(sacl)
}

// buildLabelACL returns an ACL carrying every non-label ACE of the existing
// label ACL plus one new Low no-write-up label ACE.
//
// The existing label ACEs are dropped rather than kept: a directory should
// carry exactly one integrity label, and Low is the one this mechanism
// requires. Every other SACL ACE (an audit ACE, say) is preserved, so the
// apply does not silently drop a security policy.
//
// The buffer is Go-allocated and over-sized by one ACE: AddMandatoryAce
// appends the label and updates the header's size and count.
func buildLabelACL(existing []byte, lowSID *windows.SID) ([]byte, error) {
	sidLen := windows.GetLengthSid(lowSID)
	labelAceSize := 4 + 4 + int(sidLen)

	type aceRange struct{ start, end int }
	var kept []aceRange
	keptSize := 0
	forEachACE(existing, func(aceType, aceFlags byte, mask uint32, aceStart int) bool {
		size := int(readUint16(existing, aceStart+2))
		if aceType == systemMandatoryLabelAceType {
			return true
		}
		kept = append(kept, aceRange{start: aceStart, end: aceStart + size})
		keptSize += size
		return true
	})

	baseSize := 8 + keptSize
	buf := make([]byte, baseSize+labelAceSize)
	header := (*aclHeader)(unsafe.Pointer(&buf[0]))
	header.Revision = aclRevision
	// AclSize is the buffer's CAPACITY, not its used size: AddAce refuses with
	// ERROR_NOT_ENOUGH_MEMORY when the new ACE would not fit within it, and
	// writes the used size back on success.
	header.AclSize = uint16(len(buf))
	header.AceCount = uint16(len(kept))

	off := 8
	for _, ace := range kept {
		copy(buf[off:], existing[ace.start:ace.end])
		off += ace.end - ace.start
	}

	rc, _, callErr := procAddMandatoryAce.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(aclRevision),
		uintptr(windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT),
		uintptr(mandatoryLabelNoWriteUp),
		uintptr(unsafe.Pointer(lowSID)),
	)
	runtime.KeepAlive(buf)
	if rc == 0 {
		return nil, fmt.Errorf("AddMandatoryAce: %w", callErr)
	}
	return buf, nil
}
