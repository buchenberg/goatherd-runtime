package confinement

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/buchenberg/shepherd-runtime-go/sandbox/containment"
)

// Monitor identifiers. The set is open by design — a transmit gate or egress
// broker joins here as the plan learns to assign it.
const (
	// JailMonitor is the pre-action syscall-deny monitor.
	JailMonitor = "syscall_jail"
	// CarrierMonitor is the check-at-commit monitor (settlement/DeltaApplier).
	CarrierMonitor = "carrier_check_at_commit"
)

// MonitorAssignment names which monitor enforces one clause, with what
// fallback. It is the minimal form of dialect/permission_plan.py.
type MonitorAssignment struct {
	Monitor  string `json:"monitor"`
	Timing   string `json:"timing"`   // "pre_action" | "commit"
	Fallback string `json:"fallback"` // "enforce" | "refuse"
	Spec     string `json:"spec,omitempty"`
}

// PermissionPlan is the authority-evidence artifact: it names the monitors that
// enforce an authority declaration and is citable by digest. Two monitors, one
// placement — the interposition, not the full Tranche-4 machinery.
type PermissionPlan struct {
	MatchDigest string              `json:"match_digest"`
	Assignments []MonitorAssignment `json:"assignments"`
}

// JailPlan builds the minimal plan for a jailed placement: the syscall jail
// enforces, pre-action, refusing when no complete monitor exists.
func JailPlan(matchDigest, confinementDigest string) PermissionPlan {
	return PermissionPlan{
		MatchDigest: matchDigest,
		Assignments: []MonitorAssignment{{
			Monitor:  JailMonitor,
			Timing:   "pre_action",
			Fallback: "refuse",
			Spec:     confinementDigest,
		}},
	}
}

// Digest is a stable content digest over the assignments, for authority-evidence
// citation.
func (p PermissionPlan) Digest() string {
	payload, _ := json.Marshal(p)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// SpecDigest is a stable digest of a confinement spec's writable-root set (order
// independent), used as the jail monitor's spec identity inside a PermissionPlan.
func SpecDigest(spec containment.ConfinementSpec) string {
	roots := append([]string(nil), spec.WritableRoots...)
	sort.Strings(roots)
	payload, _ := json.Marshal(struct {
		Roots []string            `json:"roots"`
		Net   containment.NetMode `json:"net"`
	}{Roots: roots, Net: spec.Network.Mode})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// JournalRecord is one entry in the authority-evidence trail: the permission plan
// that enforced a run, citable by digest at settlement.
type JournalRecord struct {
	MatchDigest string `json:"match_digest"`
	PlanDigest  string `json:"plan_digest"`
	Monitor     string `json:"monitor"`
}

// Evidence returns the journal entry for a jailed run over spec — the record the
// runtime writes so a run's enforcement is citable by digest.
func Evidence(matchDigest string, spec containment.ConfinementSpec) JournalRecord {
	plan := JailPlan(matchDigest, SpecDigest(spec))
	return JournalRecord{MatchDigest: matchDigest, PlanDigest: plan.Digest(), Monitor: JailMonitor}
}
