package winacl

import (
	"path/filepath"
	"strings"

	"github.com/buchenberg/shepherd-runtime-go/sandbox/containment"
)

// canonicalAll canonicalizes every root, dropping empties. Canonicalization
// matters twice over on Windows: the write SID is derived from the canonical
// path (so two spellings of one root converge on one identity), and the
// security-descriptor edits are applied to the canonical path (so the grant
// lands on the directory the agent will actually write to).
func canonicalAll(roots []string) []string {
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		if root == "" {
			continue
		}
		out = append(out, containment.Canonical(root))
	}
	return out
}

// withinAnyRoot reports whether p is one of roots or lies beneath one.
func withinAnyRoot(p string, roots []string) bool {
	for _, root := range roots {
		if p == root || withinOrEqual(p, root) {
			return true
		}
	}
	return false
}

// withinOrEqual reports whether child is parent or lies beneath it.
func withinOrEqual(child, parent string) bool {
	if parent == "" {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
