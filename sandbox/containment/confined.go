package containment

import (
	"fmt"

	"github.com/buchenberg/goatherd-runtime/sandbox/confinement"
)

// LaunchConfined is the only real-execution verb on a backend: resolve → profile
// → probe (fail-closed) → launch. The probe runs first and refuses when the paddock
// is not live and grant-conformant, so a broken paddock never runs a body.
//
// Mirrors ExecutionCapability.launch_confined (_execution_capability.py:183-195):
// real execution implies a paddock-capable host, or it refuses.
func LaunchConfined(b Backend, spec confinement.ConfinementSpec, req LaunchRequest) (LaunchResult, error) {
	if b == nil {
		return LaunchResult{}, fmt.Errorf(
			"%w: no paddock-capable backend on this host; real execution refuses rather than running unconfined",
			ErrPaddockNotEstablished)
	}
	roots, network, err := confinement.ResolveSpec(spec)
	if err != nil {
		return LaunchResult{}, err
	}
	if ok, why := b.Available(); !ok {
		return LaunchResult{}, fmt.Errorf("%w: %s", ErrPaddockNotEstablished, why)
	}
	if !b.SupportsNetwork(network.Mode) {
		return LaunchResult{}, fmt.Errorf("%w: backend %s cannot enforce network mode %q",
			confinement.ErrUnsupportedConfinementSpec, b.Name(), network.Mode)
	}
	profile := b.ProfileFor(roots, network)
	if err := b.Probe(profile, req.WorkingRoot, roots); err != nil {
		return LaunchResult{}, err
	}
	return b.Launch(profile, req)
}

// Capability is the per-run execution handle: the proof that a paddock-capable
// backend exists. Launch refuses when none is bound — the structural analogue of
// ExecutionCapability in _execution_capability.py.
type Capability struct {
	Backend     Backend
	WorkingRoot string
	Isolation   string // "isolated" | "ground"
}

// LaunchConfined runs req under spec, or refuses when no backend is bound.
func (c Capability) LaunchConfined(spec confinement.ConfinementSpec, req LaunchRequest) (LaunchResult, error) {
	if req.WorkingRoot == "" {
		req.WorkingRoot = c.WorkingRoot
	}
	return LaunchConfined(c.Backend, spec, req)
}

// RequireAuthority is the negotiation guard as a method: it returns
// ErrExecutionAuthorityRequired when the capability carries no backend. An
// execution-bound handler calls it (or RequireExecutionAuthority) before doing
// real work, so a capability-less dispatch refuses rather than running
// unconfined.
func (c Capability) RequireAuthority() error {
	return RequireExecutionAuthority(&c)
}

// RequireExecutionAuthority is the negotiation guard: a real-execution dispatch
// that carries no capability refuses with ErrExecutionAuthorityRequired — the
// fail-closed rule under version skew (a coordinator that does not hand out a
// capability) or a non-execution path. It never falls back to running unconfined.
//
// It is distinct from LaunchConfined's ErrPaddockNotEstablished: this guard fires
// when the capability itself is absent (dispatch-time), that one when a bound
// capability's host cannot build a paddock (enforcement-time). Mirrors the
// ExecutionAuthorityRequired negotiation rule in _execution_capability.py:62-71,
// 227-273.
func RequireExecutionAuthority(c *Capability) error {
	if c == nil || c.Backend == nil {
		return fmt.Errorf(
			"%w: real execution dispatched without an execution capability (version skew, or a non-execution path) — refusing rather than running unconfined",
			ErrExecutionAuthorityRequired)
	}
	return nil
}
