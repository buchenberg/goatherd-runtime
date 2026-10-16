package containment

import (
	"fmt"
	"path/filepath"
)

// NetMode is the coarse network axis of a confinement spec.
type NetMode string

const (
	// NetDenyAll is no egress.
	NetDenyAll NetMode = "deny_all"
	// NetAllowAll is unrestricted egress.
	NetAllowAll NetMode = "allow_all"
	// NetBroker is host-filtered egress — the egress broker's job, not the paddock's.
	NetBroker NetMode = "broker"
)

// NetworkPolicy is the network axis — coarse today, host-granular later. The paddock
// is host-blind (a filesystem LSM cannot filter by host) and ignores it; a broker
// consumes AllowedHosts.
type NetworkPolicy struct {
	Mode         NetMode
	AllowedHosts []string
	BrokerPort   int
}

// ConfinementSpec is the generic, may=-agnostic confinement policy. The authority
// layer (package confinement) lowers to this; the paddock backend enforces it.
type ConfinementSpec struct {
	WritableRoots []string
	Network       NetworkPolicy
}

// ReadOnlySpec is the ReadOnly lowering: no writable root at all (every write
// denied), no egress.
func ReadOnlySpec() ConfinementSpec {
	return ConfinementSpec{Network: NetworkPolicy{Mode: NetDenyAll}}
}

// PermissiveFor is the Permissive lowering: the working path writable, all egress
// (the documented coarse network hole; host filtering arrives with the broker).
func PermissiveFor(workingPath string) ConfinementSpec {
	return ConfinementSpec{
		WritableRoots: []string{workingPath},
		Network:       NetworkPolicy{Mode: NetAllowAll},
	}
}

// ResolveSpec resolves a spec to the paddock's inputs: canonical writable roots
// and the network axis. It validates the mode and, for NetBroker, requires the
// broker port the paddock pins egress to. Whether the *backend* can enforce the
// mode is checked separately (Backend.SupportsNetwork), fail-closed.
//
// Mirrors _resolve_spec in _execution_capability.py, extended with the broker
// port pin (Plan 4).
func ResolveSpec(spec ConfinementSpec) (roots []string, network NetworkPolicy, err error) {
	network = spec.Network
	if network.Mode == "" {
		network.Mode = NetAllowAll
	}
	switch network.Mode {
	case NetAllowAll, NetDenyAll:
	case NetBroker:
		if network.BrokerPort <= 0 {
			return nil, NetworkPolicy{}, fmt.Errorf("%w: NetBroker requires a broker port to pin egress to", ErrUnsupportedConfinementSpec)
		}
	default:
		return nil, NetworkPolicy{}, fmt.Errorf("%w: unknown network mode %q", ErrUnsupportedConfinementSpec, network.Mode)
	}
	roots = make([]string, 0, len(spec.WritableRoots))
	for _, root := range spec.WritableRoots {
		roots = append(roots, Canonical(root))
	}
	return roots, network, nil
}

// Canonical resolves a path with symlinks resolved and made absolute, mirroring
// os.path.realpath in the Python reference. A path whose symlinks cannot be
// resolved (e.g. it does not exist yet) falls back to an absolute clean.
func Canonical(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		if abs, err := filepath.Abs(resolved); err == nil {
			return abs
		}
		return resolved
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
