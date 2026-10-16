package landlock

import (
	"encoding/json"

	"github.com/buchenberg/shepherd-runtime-go/sandbox/containment"
)

// canonicalAll canonicalizes each path (symlinks resolved, made absolute),
// mirroring os.path.realpath in the Python reference. Landlock's PATH_BENEATH
// binds to the opened directory's inode, so an unresolved `..`/symlink in a spec
// would bind the wrong directory.
func canonicalAll(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, containment.Canonical(p))
	}
	return out
}

// parseRoots decodes a backend-private profile: a JSON array of writable roots.
// An empty profile ("[]") yields an empty, non-nil slice.
func parseRoots(profile string) []string {
	roots := []string{}
	if err := json.Unmarshal([]byte(profile), &roots); err != nil {
		return []string{}
	}
	return roots
}
