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
//
// There is one identity per root and no separate one for a temporary
// directory: a root is a workspace or a worktree, and two sessions that
// share neither are already isolated from each other by their roots'
// identities.
func WriteSidFor(canonicalRoot string) string {
	digest := sha256.Sum256([]byte(canonicalRoot))
	first := binary.LittleEndian.Uint32(digest[0:4])%maxSubAuthority + 1
	second := binary.LittleEndian.Uint32(digest[4:8])%maxSubAuthority + 1
	return fmt.Sprintf("S-%d-%d-%d-%d", 1, sidAuthority, first, second)
}
