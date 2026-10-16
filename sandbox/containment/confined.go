package containment

import "fmt"

// LaunchConfined is the only real-execution verb on a backend: resolve → profile
// → probe (fail-closed) → launch. The probe runs first and refuses when the jail
// is not live and grant-conformant, so a broken jail never runs a body.
//
// Mirrors ExecutionCapability.launch_confined (_execution_capability.py:183-195):
// real execution implies a jail-capable host, or it refuses.
func LaunchConfined(b Backend, spec ConfinementSpec, req LaunchRequest) (LaunchResult, error) {
	if b == nil {
		return LaunchResult{}, fmt.Errorf(
			"%w: no jail-capable backend on this host; real execution refuses rather than running unconfined",
			ErrJailNotEstablished)
	}
	roots, allowNetwork, err := ResolveSpec(spec)
	if err != nil {
		return LaunchResult{}, err
	}
	if ok, why := b.Available(); !ok {
		return LaunchResult{}, fmt.Errorf("%w: %s", ErrJailNotEstablished, why)
	}
	profile := b.ProfileFor(roots, allowNetwork)
	if err := b.Probe(profile, req.WorkingRoot, roots); err != nil {
		return LaunchResult{}, err
	}
	return b.Launch(profile, req)
}

// Capability is the per-run execution handle: the proof that a jail-capable
// backend exists. Launch refuses when none is bound — the structural analogue of
// ExecutionCapability in _execution_capability.py.
type Capability struct {
	Backend     Backend
	WorkingRoot string
	Isolation   string // "isolated" | "ground"
}

// LaunchConfined runs req under spec, or refuses when no backend is bound.
func (c Capability) LaunchConfined(spec ConfinementSpec, req LaunchRequest) (LaunchResult, error) {
	if req.WorkingRoot == "" {
		req.WorkingRoot = c.WorkingRoot
	}
	return LaunchConfined(c.Backend, spec, req)
}

// RequireExecutionAuthority is the negotiation guard: a real-execution dispatch
// that carries no capability refuses with ErrExecutionAuthorityRequired — the
// fail-closed rule under version skew (a coordinator that does not hand out a
// capability) or a non-execution path. It never falls back to running unconfined.
//
// It is distinct from LaunchConfined's ErrJailNotEstablished: this guard fires
// when the capability itself is absent (dispatch-time), that one when a bound
// capability's host cannot build a jail (enforcement-time). Mirrors the
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
