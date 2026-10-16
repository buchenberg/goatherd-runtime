package confinement

import (
	"errors"
)

// Declaration is an authority declaration: either a whole-workspace may=
// profile or a set of per-binding grants (mutually exclusive). It is the
// Go-native replacement for Python's task-signature annotations — plain data a
// host, task config, or policy builds, which the compiler turns into a plan.
//
// The declaration is the only producer of writable roots; Install is the only
// consumer that turns it into a PermissionPlan.
type Declaration struct {
	// May is a whole-workspace profile ("ReadOnly", "Permissive"); "" defaults
	// to Permissive. Mutually exclusive with Grants.
	May string
	// Grants are per-binding roots with ReadOnly/ReadWrite authority.
	// Mutually exclusive with May.
	Grants []BindingRootGrant
	// Match is the authority-surface digest folded into the plan's evidence.
	Match string
}

// MayDeclaration declares a whole-workspace profile.
func MayDeclaration(may, match string) Declaration {
	return Declaration{May: may, Match: match}
}

// GrantsDeclaration declares per-binding grants. An empty grant list is a valid
// declaration — it lowers to ReadOnly (no writable root), not to the Permissive
// default; the distinction from "no declaration" (a zero Declaration) is the
// non-nil Grants slice.
func GrantsDeclaration(match string, grants ...BindingRootGrant) Declaration {
	if grants == nil {
		grants = []BindingRootGrant{}
	}
	return Declaration{Grants: grants, Match: match}
}

// Install lowers a declaration to a two-monitor PermissionPlan: the paddock monitor
// carries the lowered ConfinementSpec; the carrier check-at-commit monitor is the
// reversible-write completeness backstop. Both underlying lowerings fail closed,
// so Install never returns a plan whose paddock clause is un-enforceable.
//
// Mirrors dialect/permission_plan.py:install — the seam where the paddock lane and
// the carrier lane meet in one plan instead of running side by side.
func Install(decl Declaration, workingPath string) (PermissionPlan, error) {
	if decl.May != "" && decl.Grants != nil {
		return PermissionPlan{}, errors.New("confinement: a declaration cannot set both May and Grants")
	}
	if decl.Grants != nil {
		// Per-binding grants — roots already absolute. An empty set is ReadOnly
		// (no writable root), matching lower_grants_to_confinement([]); it does
		// not fall through to the Permissive default.
		spec, err := LowerGrants(decl.Grants)
		if err != nil {
			return PermissionPlan{}, err
		}
		return PlanForSpec(spec, decl.Match), nil
	}
	spec, err := LowerMay(decl.May, workingPath)
	if err != nil {
		return PermissionPlan{}, err
	}
	return PlanForSpec(spec, decl.Match), nil
}
