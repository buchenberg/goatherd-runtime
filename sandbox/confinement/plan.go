package confinement

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/buchenberg/goatherd-runtime/sandbox/containment"
)

// Monitor identifiers. The set is open by design — a transmit gate or egress
// broker joins here as the plan learns to assign it.
const (
	// PaddockMonitor is the pre-action syscall-deny monitor.
	PaddockMonitor = "syscall_paddock"
	// CarrierMonitor is the check-at-commit monitor (settlement / DeltaApplier).
	CarrierMonitor = "carrier_check_at_commit"
)

// Timing values for a monitor assignment.
const (
	TimingPreAction = "pre_action"
	TimingCommit    = "commit"
)

// ErrPlanEvidence is returned by ValidatePlanEvidence when a plan's cited
// evidence does not match its descriptor, route, or digests.
var ErrPlanEvidence = errors.New("confinement: permission-plan evidence invalid")

// Evidence is one (key, digest) pair attached to a monitor assignment.
type Evidence struct {
	Key    string `json:"key"`
	Digest string `json:"digest"`
}

// MonitorAssignment names one monitor made responsible for a clause, with the
// basis that makes it real. CompletenessBasis states which routes the monitor
// covers; TamperBasis states why the judged body cannot disable it.
// Confinement is the paddock monitor's lowered spec; monitors that do not lower to
// a spec (the carrier gate) leave it nil.
//
// Mirrors dialect/permission_plan.py:MonitorAssignment.
type MonitorAssignment struct {
	Monitor           string                       `json:"monitor"`
	Timing            string                       `json:"timing"`
	CompletenessBasis string                       `json:"completeness_basis"`
	TamperBasis       string                       `json:"tamper_basis"`
	Confinement       *containment.ConfinementSpec `json:"confinement,omitempty"`
	Route             string                       `json:"route,omitempty"`
	Evidence          []Evidence                   `json:"evidence,omitempty"`
}

// PermissionPlan is the installed monitor assignment for one authority
// declaration on one placement. It is a value: equal declarations on equal
// placements produce equal plans, and Digest cites the plan as authority
// evidence. Fallback is the compiler's disposition when a clause cannot be made
// enforcement-bearing ("enforce" — the lowerings fail closed).
//
// Mirrors dialect/permission_plan.py:PermissionPlan.
type PermissionPlan struct {
	Fallback    string              `json:"fallback"`
	Assignments []MonitorAssignment `json:"assignments"`
}

// The monitor bases. They are strings, not behaviour: the plan records *who*
// enforces *what*, citable by digest.
const (
	paddockCompleteness = "managed-filesystem writes on a confined placement (Landlock deny-closed over the writable roots)"
	paddockTamper       = "the paddock is applied in the confined child before exec, outside the task process (PR_SET_NO_NEW_PRIVS + Landlock RestrictSelf)"
	carrierCompleteness = "reversible filesystem writes captured in the carrier working-tree delta, proposed at return before merge; the backstop for managed writes that bypass cooperative paths"
	carrierTamper       = "the check runs in the coordinator/reversible wrap, outside the task process"
)

// CarrierCheckAuthority is the authority declaration for a carrier-only plan
// (a check-at-commit route with no paddock clause).
type CarrierCheckAuthority struct {
	EffectiveMatchDigest       string
	AuthoritySurfacePlanDigest string
	Route                      string
	CompletenessBasis          string
	TamperBasis                string
}

// PlanForSpec builds a two-monitor plan from an already-lowered confinement
// spec: the paddock monitor carries the spec; the carrier monitor is the
// reversible-write completeness backstop. matchDigest, when non-empty, is
// folded into the carrier assignment's evidence (effective_match_digest), the
// authority-surface provenance a settlement can re-validate. It is the
// behaviour-preserving core Install uses, and what a caller that already holds
// a spec (the git backend) cites for evidence.
func PlanForSpec(spec containment.ConfinementSpec, matchDigest string) PermissionPlan {
	s := spec
	carrier := MonitorAssignment{
		Monitor:           CarrierMonitor,
		Timing:            TimingCommit,
		CompletenessBasis: carrierCompleteness,
		TamperBasis:       carrierTamper,
	}
	if matchDigest != "" {
		carrier.Evidence = []Evidence{{Key: "effective_match_digest", Digest: matchDigest}}
	}
	return PermissionPlan{
		Fallback: "enforce",
		Assignments: []MonitorAssignment{
			{
				Monitor:           PaddockMonitor,
				Timing:            TimingPreAction,
				CompletenessBasis: paddockCompleteness,
				TamperBasis:       paddockTamper,
				Confinement:       &s,
			},
			carrier,
		},
	}
}

// InstallCarrierCheck builds a carrier-only plan.
func InstallCarrierCheck(a CarrierCheckAuthority) PermissionPlan {
	route := a.Route
	if route == "" {
		route = "carrier_diff"
	}
	basis := a.CompletenessBasis
	if basis == "" {
		basis = carrierCompleteness
	}
	tamper := a.TamperBasis
	if tamper == "" {
		tamper = carrierTamper
	}
	var evidence []Evidence
	if a.EffectiveMatchDigest != "" {
		evidence = append(evidence, Evidence{Key: "effective_match_digest", Digest: a.EffectiveMatchDigest})
	}
	if a.AuthoritySurfacePlanDigest != "" {
		evidence = append(evidence, Evidence{Key: "authority_surface_plan_digest", Digest: a.AuthoritySurfacePlanDigest})
	}
	return PermissionPlan{
		Fallback: "enforce",
		Assignments: []MonitorAssignment{{
			Monitor:           CarrierMonitor,
			Timing:            TimingCommit,
			CompletenessBasis: basis,
			TamperBasis:       tamper,
			Route:             route,
			Evidence:          evidence,
		}},
	}
}

// Monitor returns the assignment for name, if the plan assigns it.
func (p PermissionPlan) Monitor(name string) (MonitorAssignment, bool) {
	for _, a := range p.Assignments {
		if a.Monitor == name {
			return a, true
		}
	}
	return MonitorAssignment{}, false
}

// Confinement returns the paddock monitor's spec — the behaviour-preserving
// output the run hands to LaunchConfined. It errors when the plan assigns no
// paddock monitor (a carrier-only plan).
func (p PermissionPlan) Confinement() (containment.ConfinementSpec, error) {
	a, ok := p.Monitor(PaddockMonitor)
	if !ok || a.Confinement == nil {
		return containment.ConfinementSpec{}, fmt.Errorf("%w: plan has no paddock monitor assignment carrying a spec", ErrPlanEvidence)
	}
	return *a.Confinement, nil
}

// Descriptor is the stable, JSON-shaped monitor plan, the artifact Digest hashes.
func (p PermissionPlan) Descriptor() map[string]any {
	assignments := make([]any, 0, len(p.Assignments))
	for _, a := range p.Assignments {
		assignments = append(assignments, a.descriptor())
	}
	return map[string]any{
		"schema":      "goatherd.permission-plan.v1",
		"fallback":    p.Fallback,
		"assignments": assignments,
	}
}

// descriptor is the assignment's JSON-shaped form.
func (a MonitorAssignment) descriptor() map[string]any {
	d := map[string]any{
		"monitor":            a.Monitor,
		"timing":             a.Timing,
		"completeness_basis": a.CompletenessBasis,
		"tamper_basis":       a.TamperBasis,
	}
	if a.Confinement != nil {
		d["confinement"] = map[string]any{
			"writable_roots": sortedCopy(a.Confinement.WritableRoots),
			"network": map[string]any{
				"mode":          string(a.Confinement.Network.Mode),
				"allowed_hosts": sortedCopy(a.Confinement.Network.AllowedHosts),
				"broker_port":   a.Confinement.Network.BrokerPort,
			},
		}
	} else {
		d["confinement"] = nil
	}
	if a.Route != "" {
		d["route"] = a.Route
	}
	if len(a.Evidence) > 0 {
		ev := make(map[string]string, len(a.Evidence))
		for _, e := range a.Evidence {
			ev[e.Key] = e.Digest
		}
		d["evidence"] = ev
	}
	return d
}

// Digest is a stable content digest over the descriptor, for authority-evidence
// citation.
func (p PermissionPlan) Digest() string {
	payload, _ := json.Marshal(p.Descriptor())
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// ValidatePlanEvidence re-checks that digest, route, and the cited match /
// surface digests agree with the plan — the tamper check a settlement runs
// before trusting a cited plan.
func ValidatePlanEvidence(digest string, plan PermissionPlan, route, matchDigest, surfaceDigest string) error {
	if digest == "" {
		return fmt.Errorf("%w: digest is required", ErrPlanEvidence)
	}
	if plan.Digest() != digest {
		return fmt.Errorf("%w: plan digest %q does not match the plan", ErrPlanEvidence, digest)
	}
	if route != "" {
		carrier, ok := plan.Monitor(CarrierMonitor)
		if !ok || carrier.Route != route {
			return fmt.Errorf("%w: carrier route %q not found in the plan", ErrPlanEvidence, route)
		}
	}
	if err := checkEvidence(plan, "effective_match_digest", matchDigest); err != nil {
		return err
	}
	return checkEvidence(plan, "authority_surface_plan_digest", surfaceDigest)
}

// checkEvidence verifies a cited digest appears among the plan's evidence when
// a non-empty value is claimed.
func checkEvidence(plan PermissionPlan, key, want string) error {
	if want == "" {
		return nil
	}
	for _, a := range plan.Assignments {
		for _, e := range a.Evidence {
			if e.Key == key {
				if e.Digest != want {
					return fmt.Errorf("%w: %s = %q, want %q", ErrPlanEvidence, key, e.Digest, want)
				}
				return nil
			}
		}
	}
	return fmt.Errorf("%w: plan cites no %s evidence", ErrPlanEvidence, key)
}

// SpecDigest is a stable digest of a confinement spec's writable-root set and
// network mode, independent of ordering.
func SpecDigest(spec containment.ConfinementSpec) string {
	payload, _ := json.Marshal(map[string]any{
		"roots": sortedCopy(spec.WritableRoots),
		"net":   string(spec.Network.Mode),
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// sortedCopy returns a sorted copy so digests are order-independent.
func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
