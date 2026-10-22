package confinement

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// ErrOverlappingBoundRoots: bound roots overlap or nest — refuse, because the
// syscall paddock's writable-root rules are additive, so a ReadOnly root nested
// inside a ReadWrite root would be silently writable. Whole-root-per-binding
// soundness therefore requires disjoint roots; overlap fails closed here.
var ErrOverlappingBoundRoots = errors.New("confinement: bound roots overlap, alias, or nest")

// ValidateDisjointRoots fails closed unless every bound root is disjoint from the
// others, returning the canonicalized (realpath) roots on success. Overlap is
// judged by three lenses (confinement.py:_roots_overlap): realpath
// equality/nesting, inode identity for roots that exist, and NFC-normalized,
// lower-cased equality/nesting for aliases whose targets do not exist yet.
//
// The fold is NFC + casefold, mirroring Python's
// unicodedata.normalize("NFC", part).casefold() (via
// golang.org/x/text/unicode/norm).
func ValidateDisjointRoots(roots []string) ([]string, error) {
	canonical := make([]string, 0, len(roots))
	for _, root := range roots {
		canonical = append(canonical, Canonical(root))
	}
	for i := range canonical {
		for j := i + 1; j < len(canonical); j++ {
			if rootsOverlap(canonical[i], canonical[j]) {
				return nil, fmt.Errorf(
					"%w: %s vs %s — per-binding grants require disjoint roots (a nested or filesystem-aliased root is sub-root semantics)",
					ErrOverlappingBoundRoots, canonical[i], canonical[j])
			}
		}
	}
	return canonical, nil
}

// rootsOverlap reports whether two realpath'd roots name the same subtree or one
// nests inside the other.
func rootsOverlap(a, b string) bool {
	if a == b || pathWithin(a, b) || pathWithin(b, a) {
		return true
	}
	if fa, err := os.Stat(a); err == nil {
		if fb, err := os.Stat(b); err == nil && os.SameFile(fa, fb) {
			return true // same inode ⇒ same directory, regardless of case/normalization
		}
	}
	fa, fb := foldPath(a), foldPath(b)
	return fa == fb || pathWithin(fa, fb) || pathWithin(fb, fa)
}

// pathWithin reports whether child is parent or lies beneath it.
func pathWithin(child, parent string) bool {
	if parent == "" {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// foldPath NFC-normalizes then lower-cases each path component for
// aliasing-aware comparison, mirroring Python's NFC + casefold.
func foldPath(p string) string {
	parts := strings.Split(filepath.Clean(p), string(filepath.Separator))
	for i, part := range parts {
		parts[i] = strings.ToLower(norm.NFC.String(part))
	}
	return strings.Join(parts, string(filepath.Separator))
}
