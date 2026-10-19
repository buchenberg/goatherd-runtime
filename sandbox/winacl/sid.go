package winacl

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// sidAuthority is the identifier authority of the S-1-4-* form: NT authority
// with two subauthorities. The form names no account, so a SID's power is
// defined solely by the ACEs that name it.
const sidAuthority = 4

// maxSubAuthority is the largest value a subauthority may hold (2^30 - 1).
const maxSubAuthority = 1<<30 - 1

// WriteSidFor derives the deterministic write identity of a root.
//
// The input MUST be the canonical root path (containment.Canonical): the
// derivation is over the path text, so two spellings of one root would
// otherwise mint two identities and grant the same directory twice under
// different SIDs. Canonicalization converges case and alias spellings, so one
// root yields one identity.
//
// The derivation is deterministic, which buys a standing-grant reuse cache: the
// ACE for a given root materializes once per root per machine, and every later
// provision detects the exact ACE and skips the (eager, tree-wide) security
// descriptor write. Renaming a root derives a new SID and leaves the old
// ACEs as inert residue.
//
// The SID is not a secret. Anyone can derive it; what it confers is the ACEs
// on the granted roots, and only tokens minted for that root carry it.
func WriteSidFor(canonicalRoot string) string {
	return sidFromDigest(sha256.Sum256([]byte(canonicalRoot)), 0)
}

// TempWriteSid derives the write identity of a private temporary directory.
//
// A private temp directory is a different capability from the workspace it
// sits beside, so it gets a different identity: sharing one would let sibling
// sessions write one another's temp trees. The fixed leading subauthority
// domain-separates the result from every workspace SID, and the directory path
// itself supplies the entropy.
func TempWriteSid(canonicalDir string) string {
	digest := sha256.Sum256(append([]byte("temp\x00"), []byte(canonicalDir)...))
	return sidFromDigest(digest, 1)
}

// sidFromDigest renders two digest words as an S-1-4-x-y SID. The third
// argument is the number of leading subauthorities the form carries: 2 for a
// workspace SID (S-1-4-x-y) and 3 for a domain-separated temp SID
// (S-1-4-x-y-1).
func sidFromDigest(digest [32]byte, extraSubAuthorities int) string {
	first := binary.LittleEndian.Uint32(digest[0:4])%maxSubAuthority + 1
	second := binary.LittleEndian.Uint32(digest[4:8])%maxSubAuthority + 1
	sid := fmt.Sprintf("S-%d-%d-%d-%d", 1, sidAuthority, first, second)
	for i := 0; i < extraSubAuthorities; i++ {
		sid += "-1"
	}
	return sid
}
