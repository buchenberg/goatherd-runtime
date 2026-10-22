package landlock

import (
	"encoding/json"
	"github.com/buchenberg/goatherd-runtime/sandbox/confinement"
)

// netPolicy is the backend-private network pin within a profile.
type netPolicy struct {
	Mode string // "allow_all" | "deny_all" | "broker"
	Port int    // broker: the pinned egress port
}

// netDoc is the profile's network section.
type netDoc struct {
	Mode string `json:"mode"`
	Port int    `json:"port,omitempty"`
}

// profileDoc is the backend-private profile: canonical writable roots plus the
// network pin. It is opaque to callers and never crosses implementations.
type profileDoc struct {
	Roots []string `json:"roots"`
	Net   netDoc   `json:"net"`
}

// canonicalAll canonicalizes each path (symlinks resolved, made absolute),
// mirroring os.path.realpath in the Python reference. Landlock's PATH_BENEATH
// binds to the opened directory's inode, so an unresolved `..`/symlink in a spec
// would bind the wrong directory.
func canonicalAll(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, confinement.Canonical(p))
	}
	return out
}

// parseProfile decodes a backend-private profile. A malformed profile — or an
// omitted mode — defaults to deny_all, never a trusted allow-all guess.
func parseProfile(profile string) profileDoc {
	var doc profileDoc
	if err := json.Unmarshal([]byte(profile), &doc); err != nil {
		return profileDoc{Roots: []string{}, Net: netDoc{Mode: "deny_all"}}
	}
	if doc.Roots == nil {
		doc.Roots = []string{}
	}
	if doc.Net.Mode == "" {
		doc.Net.Mode = "deny_all"
	}
	return doc
}

// netPolicyOf returns the network pin from a profile doc.
func netPolicyOf(d netDoc) netPolicy { return netPolicy{Mode: d.Mode, Port: d.Port} }
