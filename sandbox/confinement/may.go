// Package confinement is the authority-enforcement seam: it lowers an authority
// declaration — a may= profile or a set of per-binding grants — to a containment
// spec. It is the Go port of the dialect's confinement.py: the one place that
// turns "what the caller declared" into "which roots are writable", fail-closed.
//
// The lowering is deliberately whole-root per binding (∅-or-all), which keeps it
// sound without sub-root semantics: writable-root rules are additive, so a
// ReadOnly root nested inside a ReadWrite root would be silently writable;
// disjointness is what makes whole-root-per-binding sound.
package confinement

import (
	"errors"
	"fmt"

	"github.com/buchenberg/goatherd-runtime/sandbox/containment"
)

// ErrUnsupportedMayProfile is a declared may= profile with no v0 lowering —
// refuse, never weaken (confinement.py:UnsupportedMayProfileError).
var ErrUnsupportedMayProfile = errors.New("confinement: may= profile has no v0 lowering")

// MayResolution records the lowering's provenance: what the author wrote vs what
// the run got, so a defaulted-Permissive run never masquerades as a declared one.
type MayResolution struct {
	Declared string // the author's declaration ("" = omitted)
	Resolved string // the profile the run lowers to
	Source   string // "declared" | "defaulted"
}

// ResolveMay is the single default site: an omitted may= resolves to Permissive.
func ResolveMay(may string) MayResolution {
	if may == "" {
		return MayResolution{Resolved: "Permissive", Source: "defaulted"}
	}
	return MayResolution{Declared: may, Resolved: may, Source: "declared"}
}

// LowerMay lowers a declared profile name to a confinement spec.
func LowerMay(may, workingPath string) (containment.ConfinementSpec, error) {
	return LowerMayResolution(ResolveMay(may), workingPath)
}

// LowerMayResolution lowers an already-resolved may= declaration.
func LowerMayResolution(res MayResolution, workingPath string) (containment.ConfinementSpec, error) {
	switch res.Resolved {
	case "Permissive":
		return containment.PermissiveFor(workingPath), nil
	case "ReadOnly":
		return containment.ReadOnlySpec(), nil
	default:
		return containment.ConfinementSpec{}, fmt.Errorf(
			"%w: may=%q (live profiles: 'ReadOnly', 'Permissive'; 'Standard'/'ModelOnly' are signposted, not built)",
			ErrUnsupportedMayProfile, res.Declared)
	}
}

// BindingRootGrant is one bound root and whether its per-binding grant makes the
// subtree writable. The pure input to per-binding paddock lowering, decoupled from
// the signature-annotation machinery that produces it.
type BindingRootGrant struct {
	Binding  string
	Root     string
	Writable bool // true for a ReadWrite grant, false for ReadOnly
}

// LowerGrants lowers per-binding grants to a deny-closed multi-root spec: the
// union of the ReadWrite roots is writable; ReadOnly roots contribute no writable
// root (their subtree is denied at the syscall). The network axis defaults to
// deny-all — v0.2 makes no network claim. Bound roots must be disjoint.
//
// Mirrors confinement.py:lower_grants_to_confinement.
func LowerGrants(grants []BindingRootGrant) (containment.ConfinementSpec, error) {
	roots := make([]string, 0, len(grants))
	for _, g := range grants {
		roots = append(roots, g.Root)
	}
	if _, err := ValidateDisjointRoots(roots); err != nil {
		return containment.ConfinementSpec{}, err
	}
	writable := make([]string, 0, len(grants))
	for _, g := range grants {
		if g.Writable {
			writable = append(writable, containment.Canonical(g.Root))
		}
	}
	return containment.ConfinementSpec{
		WritableRoots: writable,
		Network:       containment.NetworkPolicy{Mode: containment.NetDenyAll},
	}, nil
}
