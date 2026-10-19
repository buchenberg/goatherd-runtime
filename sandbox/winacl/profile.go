package winacl

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The restricting-SID list selection. The mode names the shape of the
// restriction, not the filesystem permission: "workspace-write" means the
// token carries one write capability SID per granted root, and "read-only"
// means it carries none, so every write is denied.
const (
	modeReadOnly       = "read-only"
	modeWorkspaceWrite = "workspace-write"
)

// netDoc is the network pin recorded in the profile. The mechanism has no
// egress control, so the pin is recorded for the evidence trail and for the
// refusal decision, never for enforcement.
type netDoc struct {
	Mode string `json:"mode"`
	Port int    `json:"port"`
}

// profile is the backend-private profile: the canonical writable roots, the
// per-root write capability SIDs, the restricting mode, and the network pin.
//
// The SIDs travel with the roots rather than being re-derived by the runner
// because the SID IS the authority on Windows: a profile whose SIDs disagree
// with its roots would grant one root's authority to another. The runner
// re-derives and compares them, so a mismatched profile is refused rather
// than applied.
type profile struct {
	Roots []string `json:"roots"`
	SIDs  []string `json:"sids"`
	Mode  string   `json:"mode"`
	Net   netDoc   `json:"net"`
}

// encode renders the profile as the backend-private JSON document the runner
// consumes. A marshal failure cannot happen for this shape, but the fallback
// is a profile with no roots — the most restrictive reading — rather than a
// permissive one.
func (p profile) encode() string {
	payload, err := json.Marshal(p)
	if err != nil {
		return `{"roots":[],"sids":[],"mode":"read-only","net":{"mode":"deny_all"}}`
	}
	return string(payload)
}

// decodeProfile parses a profile document.
func decodeProfile(doc string) (profile, error) {
	var p profile
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		return profile{}, fmt.Errorf("winacl: parse profile: %w", err)
	}
	return p, nil
}

// validate checks the profile's internal consistency, failing closed. A
// profile whose SIDs do not match its roots is refused: applying it would
// grant one root's write authority over another.
func (p profile) validate() error {
	switch p.Mode {
	case modeReadOnly:
		if len(p.SIDs) != 0 {
			return fmt.Errorf("winacl: read-only profile carries %d write SIDs", len(p.SIDs))
		}
	case modeWorkspaceWrite:
		if len(p.SIDs) != len(p.Roots) {
			return fmt.Errorf("winacl: profile has %d roots but %d write SIDs", len(p.Roots), len(p.SIDs))
		}
	default:
		return fmt.Errorf("winacl: unknown profile mode %q", p.Mode)
	}
	for i, root := range p.Roots {
		if root == "" {
			return fmt.Errorf("winacl: profile root %d is empty", i)
		}
		if !strings.EqualFold(p.SIDs[i], WriteSidFor(root)) {
			return fmt.Errorf("winacl: profile SID %d does not match the write identity of its root", i)
		}
	}
	return nil
}
