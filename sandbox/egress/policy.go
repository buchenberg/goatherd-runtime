// Package egress is the network-egress mediation axis: a host predicate the
// syscall jail cannot evaluate (the jail is host-blind) plus a loopback
// HTTP-CONNECT proxy that enforces it.
//
// It is the Go port of vcs-core/_egress_broker.py. Egress is irreversible (no
// undo, no compensation), so per the check-time law it is checked *before* it
// happens. The jail pins egress to the broker's loopback port (Plan 4 §8),
// making the broker the only route out — unbypassable by construction, not by
// the client's cooperation. The broker is platform-neutral (stdlib sockets);
// only the pin is per-platform.
package egress

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/buchenberg/shepherd-runtime-go/sandbox/containment"
)

// Verdict is a per-connection broker decision.
type Verdict string

const (
	// VerdictAllow forwards the connection.
	VerdictAllow Verdict = "allow"
	// VerdictDeny refuses it (HTTP 403).
	VerdictDeny Verdict = "deny"
	// VerdictEscalate is enumerated for the irreversible-effect human-approval
	// gate; it is deny-loud in this MVP (the reference's escalate path is unbuilt).
	VerdictEscalate Verdict = "escalate"
)

// HostAtom is one value-level host predicate.
type HostAtom struct {
	// Op is "exact", "in", "contains", or "matches" (full-match regex).
	Op string
	// Value is a string (exact/contains/matches) or []string (in).
	Value any
}

// Matches reports whether host satisfies the atom. An unknown op is an error —
// never a silent allow.
func (a HostAtom) Matches(host string) (bool, error) {
	switch a.Op {
	case "exact":
		s, _ := a.Value.(string)
		return host == s, nil
	case "in":
		set, _ := a.Value.([]string)
		for _, s := range set {
			if host == s {
				return true, nil
			}
		}
		return false, nil
	case "contains":
		s, _ := a.Value.(string)
		return s != "" && strings.Contains(host, s), nil
	case "matches":
		s, _ := a.Value.(string)
		re, err := regexp.Compile("^(?:" + s + ")$")
		if err != nil {
			return false, fmt.Errorf("egress: host-atom regex %q: %w", s, err)
		}
		return re.MatchString(host), nil
	default:
		return false, fmt.Errorf("egress: unknown host-atom op %q (lowering error — never a silent allow)", a.Op)
	}
}

// HostGrant is one DNF disjunct: it admits a host iff a positive matches and no
// negative does. Negation is grant-local (deny-wins within the grant, never a
// cross-grant veto). Verdict classifies the admitted hosts; the zero value is
// VerdictAllow.
type HostGrant struct {
	Positive []HostAtom
	Negative []HostAtom
	Verdict  Verdict
}

// Admits reports whether the grant admits host.
func (g HostGrant) Admits(host string) (bool, error) {
	for _, a := range g.Negative {
		ok, err := a.Matches(host)
		if err != nil {
			return false, err
		}
		if ok {
			return false, nil
		}
	}
	for _, a := range g.Positive {
		ok, err := a.Matches(host)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// EgressDecision is the evidence record for one mediated connection.
type EgressDecision struct {
	Host    string  `json:"host"`
	Port    int     `json:"port"`
	Verdict Verdict `json:"verdict"`
	Kind    string  `json:"kind"`
}

// EgressPolicy is the compiled host predicate: union-across-grants of
// (positive ∧ ¬negative), default-deny, escalate→deny-loud. Pure and
// deterministic — unit-testable with no sockets.
type EgressPolicy struct {
	grants []HostGrant
}

// PolicyFromGrants builds a policy from lowered grants (the dialect seam).
func PolicyFromGrants(grants ...HostGrant) EgressPolicy {
	return EgressPolicy{grants: grants}
}

// AllowHosts is the common case: an exact-host allowlist (one ALLOW grant per
// host).
func AllowHosts(hosts ...string) EgressPolicy {
	grants := make([]HostGrant, 0, len(hosts))
	for _, h := range hosts {
		grants = append(grants, HostGrant{Positive: []HostAtom{{Op: "exact", Value: h}}})
	}
	return EgressPolicy{grants: grants}
}

// PolicyFromNetwork builds the broker policy from a network policy's allowed
// hosts — the lowering seam from the paddock's NetworkPolicy to the broker's
// host predicate. It is meaningful only for NetBroker: the paddock pins egress
// to the broker's port, and the broker decides which hosts.
func PolicyFromNetwork(np containment.NetworkPolicy) (EgressPolicy, error) {
	if np.Mode != containment.NetBroker {
		return EgressPolicy{}, fmt.Errorf("egress: a broker policy requires NetBroker mode, got %q", np.Mode)
	}
	return AllowHosts(np.AllowedHosts...), nil
}

// Decide applies the evaluation-order contract. An unknown host-atom op is an
// error (the caller fails closed).
func (p EgressPolicy) Decide(host string, port int) (EgressDecision, error) {
	allow, escalate := false, false
	for _, g := range p.grants {
		ok, err := g.Admits(host)
		if err != nil {
			return EgressDecision{}, err
		}
		if !ok {
			continue
		}
		if g.Verdict == VerdictEscalate {
			escalate = true
		} else {
			allow = true
		}
	}
	switch {
	case allow:
		return EgressDecision{Host: host, Port: port, Verdict: VerdictAllow, Kind: "broker:connect-allowed"}, nil
	case escalate:
		return EgressDecision{Host: host, Port: port, Verdict: VerdictEscalate, Kind: "broker:connect-escalate-unbuilt"}, nil
	default:
		return EgressDecision{Host: host, Port: port, Verdict: VerdictDeny, Kind: "broker:connect-denied"}, nil
	}
}
