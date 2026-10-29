package seatbelt

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/buchenberg/goatherd-runtime/sandbox/confinement"
)

// The network modes the profile can represent. The names match
// confinement.NetMode; a zero mode is read as deny_all rather than allow-all.
const (
	modeAllowAll = "allow_all"
	modeDenyAll  = "deny_all"
	modeBroker   = "broker"
)

// netDoc is the network pin within a profile.
//
// Port is meaningful only under broker: the broker's loopback port, the one
// outbound address the profile admits.
type netDoc struct {
	Mode string `json:"mode"`
	Port int    `json:"port,omitempty"`
}

// profileDoc is the backend-private profile: canonical writable roots plus the
// network pin. It is opaque to callers, exactly like the landlock and winacl
// profiles, and is rendered to SBPL only at probe/launch time.
type profileDoc struct {
	Roots []string `json:"roots"`
	Net   netDoc   `json:"net"`
}

// encodeProfile renders the profile as the backend-private JSON document.
//
// A marshal failure cannot happen for this shape, but the fallback is the
// most restrictive reading — no roots, deny-all egress — rather than a
// permissive one.
func encodeProfile(doc profileDoc) string {
	payload, err := json.Marshal(doc)
	if err != nil {
		return `{"roots":[],"net":{"mode":"deny_all"}}`
	}
	return string(payload)
}

// parseProfile decodes a backend-private profile.
//
// A malformed profile — or an omitted mode — defaults to deny_all, never a
// trusted allow-all guess. Roots are not normalized here: a profile that
// reaches validate with a relative or empty root is refused rather than
// silently cleaned into something broader.
func parseProfile(profile string) (profileDoc, error) {
	var doc profileDoc
	if err := json.Unmarshal([]byte(profile), &doc); err != nil {
		return profileDoc{}, fmt.Errorf("seatbelt: parse profile: %w", err)
	}
	if doc.Roots == nil {
		doc.Roots = []string{}
	}
	if doc.Net.Mode == "" {
		doc.Net.Mode = modeDenyAll
	}
	return doc, nil
}

// validate checks a profile's internal consistency, failing closed.
func (p profileDoc) validate() error {
	switch p.Net.Mode {
	case modeAllowAll, modeDenyAll:
		if p.Net.Port != 0 {
			return fmt.Errorf("seatbelt: network mode %q carries a port", p.Net.Mode)
		}
	case modeBroker:
		if p.Net.Port <= 0 || p.Net.Port > 65535 {
			return fmt.Errorf("seatbelt: broker mode needs a valid port, got %d", p.Net.Port)
		}
	default:
		return fmt.Errorf("seatbelt: unknown network mode %q", p.Net.Mode)
	}
	for i, root := range p.Roots {
		if root == "" {
			return fmt.Errorf("seatbelt: profile root %d is empty", i)
		}
		if !filepath.IsAbs(root) {
			return fmt.Errorf("seatbelt: profile root %d is not absolute: %q", i, root)
		}
	}
	return nil
}

// canonicalAll canonicalizes every non-empty root (symlinks resolved, made
// absolute). Canonicalization is load-bearing: Seatbelt does not resolve
// subpath symlinks, so an unresolved /var/... root denies writes to the
// /private/var/... path the agent actually uses.
func canonicalAll(roots []string) []string {
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		if root == "" {
			continue
		}
		out = append(out, confinement.Canonical(root))
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
