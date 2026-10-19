package winacl

import (
	"github.com/buchenberg/shepherd-runtime-go/sandbox/containment"
)

// Backend is the Windows write-restriction containment backend.
//
// It implements the same containment.Backend contract as the Linux Landlock
// member, so a host selects a backend per platform and passes it to the
// sandbox constructor; nothing in the root module needs to know which
// mechanism is underneath.
type Backend struct{}

// New returns a Windows write-restriction backend.
func New() *Backend { return &Backend{} }

// Detect returns a Windows write-restriction backend when the host can
// provide the paddock, or nil on a host that cannot (advisory tier only). It
// mirrors detect_containment_backend, but lives here rather than in package
// containment because the root module must not import this nested module.
func Detect() containment.Backend {
	b := New()
	if ok, _ := b.Available(); ok {
		return b
	}
	return nil
}

// Name implements containment.Backend.
func (b *Backend) Name() string { return "winacl" }

// EnforcementTier implements containment.Backend.
//
// It is "native-access-check-deny", not "native-syscall-deny": enforcement
// happens in the security reference monitor's access check, not in a syscall
// filter. The distinction is load-bearing for anyone reasoning about what the
// boundary covers — a syscall filter governs a call, an access check governs an
// object.
func (b *Backend) EnforcementTier() string { return "native-access-check-deny" }

// Available reports whether the host can establish the restriction.
//
// The probe is a real capability check, not a version comparison: it opens the
// process token with the rights CreateRestrictedToken needs and mints a
// restricted token. A host that cannot do that cannot confine anything, and
// the reason is reported so a caller can surface it.
func (b *Backend) Available() (bool, string) { return available() }

// SupportsNetwork reports whether the backend can enforce the given network
// mode.
//
// The mechanism has no egress control: a restricted token constrains
// filesystem writes, not sockets. Only allow_all is representable. deny_all
// and broker are refused, and LaunchConfined turns that refusal into a
// fail-closed error rather than running with the network axis silently
// ignored.
func (b *Backend) SupportsNetwork(mode containment.NetMode) bool {
	switch mode {
	case containment.NetAllowAll:
		return true
	default:
		return false
	}
}

// ProfileFor lowers canonical writable roots and the network pin to a
// backend-private profile.
//
// The mode follows the root count: no roots is read-only (no write SID at
// all, so every write is denied), one or more roots is workspace-write with
// one write capability SID per root.
func (b *Backend) ProfileFor(roots []string, network containment.NetworkPolicy) string {
	canonical := canonicalAll(roots)
	netMode := string(network.Mode)
	if netMode == "" {
		// Fail closed: an omitted mode denies egress.
		netMode = string(containment.NetDenyAll)
	}
	p := profile{
		Roots: canonical,
		Mode:  modeReadOnly,
		Net:   netDoc{Mode: netMode, Port: network.BrokerPort},
	}
	if len(canonical) > 0 {
		p.Mode = modeWorkspaceWrite
		p.SIDs = make([]string, 0, len(canonical))
		for _, root := range canonical {
			p.SIDs = append(p.SIDs, WriteSidFor(root))
		}
	}
	return p.encode()
}

// Probe proves the restriction is BOTH live and grant-conformant before the
// body runs (fail-closed). It is exit-code aware: a confinement failure is
// "no paddock", not "write denied", so a broken restriction can never pass as
// a working one.
//
// The three checks mirror the Landlock member's probe exactly: liveness (an
// out-of-workdir write must be denied), per-root conformance (a canary write
// beneath each declared root must be allowed), and deny-closed (an in-workdir
// path outside every root must be denied).
func (b *Backend) Probe(profile, workingRoot string, roots []string) error {
	return probe(profile, workingRoot, roots)
}

// Launch runs a command under the profile.
//
// It does not classify the exit status; a non-zero body exit is returned
// as-is, and the caller maps an authority denial to ErrSandboxDenied when
// appropriate.
func (b *Backend) Launch(profile string, req containment.LaunchRequest) (containment.LaunchResult, error) {
	return launch(profile, req)
}
