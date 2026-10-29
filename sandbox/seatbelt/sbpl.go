package seatbelt

import (
	"fmt"
	"path/filepath"
	"strings"
)

// devWriteRoot is the device-tree write grant every profile carries.
//
// It is not the workspace: /dev/null and friends must stay writable for an
// ordinary body to run. The reference makes the same allowance. Unlike
// Landlock, Seatbelt's deny-closed profile is expressed with a plain subpath
// grant, so this does not imply /dev/shm is carved out.
const devWriteRoot = "/dev"

// renderSBPL renders a profile document as a Seatbelt profile.
//
// The shape is the reference lowering (vcs-core _seatbelt_containment.py):
// allow the default posture, deny all file writes, then re-allow writes
// beneath each canonical writable root and /dev. Two Go-specific additions:
//
//   - Writes beneath each root's .git are denied explicitly. A linked
//     worktree's gitdir lives outside the writable root and is already
//     denied, but a whole-repository writable root would otherwise let a
//     confined process rewrite history.
//   - NetBroker admits exactly the broker's loopback port after the general
//     network-outbound deny; the host allowlist remains the broker's job.
func renderSBPL(doc profileDoc) string {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(allow default)\n")
	b.WriteString("(deny file-write*)\n")

	switch doc.Net.Mode {
	case modeDenyAll:
		b.WriteString("(deny network-outbound)\n")
	case modeBroker:
		b.WriteString("(deny network-outbound)\n")
		// SBPL accepts only "*" or "localhost" as the host in a network
		// address; a numeric address is a profile error. "localhost" is the
		// loopback host the broker listens on.
		fmt.Fprintf(&b, "(allow network-outbound (remote ip \"localhost:%d\"))\n", doc.Net.Port)
	}

	for _, root := range doc.Roots {
		fmt.Fprintf(&b, "(allow file-write* (subpath \"%s\"))\n", sbplString(root))
	}
	fmt.Fprintf(&b, "(allow file-write* (subpath \"%s\"))\n", sbplString(devWriteRoot))

	// A repository's .git is never writable, even when its root is. These
	// rules come last so they win under the profile's rule ordering: the
	// root grant above is broader, and a later matching rule overrides it.
	// The literal grant covers a linked worktree's .git *file*; the subpath
	// grant covers a normal repository's .git directory.
	for _, root := range doc.Roots {
		git := filepath.Join(root, ".git")
		fmt.Fprintf(&b, "(deny file-write* (literal \"%s\"))\n", sbplString(git))
		fmt.Fprintf(&b, "(deny file-write* (subpath \"%s\"))\n", sbplString(git))
	}

	return b.String()
}

// sbplString escapes a path for a double-quoted SBPL string literal.
//
// SBPL literals use C-style escapes; a raw backslash or double quote can
// terminate or derail the literal, so both are escaped (backslash first).
// A workspace path is attacker-influenced in the threat model this backend
// exists for, so the profile must never depend on raw interpolation.
func sbplString(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return strings.ReplaceAll(value, `"`, `\"`)
}
