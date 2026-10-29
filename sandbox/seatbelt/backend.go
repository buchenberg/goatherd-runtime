package seatbelt

import (
	"github.com/buchenberg/goatherd-runtime/sandbox/confinement"
	"github.com/buchenberg/goatherd-runtime/sandbox/containment"
)

// Backend is the macOS Seatbelt containment backend.
//
// It implements the same containment.Backend contract as the Linux Landlock
// and Windows winacl members, so a host selects a backend per platform and
// passes it to the sandbox constructor; nothing in the root module needs to
// know which mechanism is underneath.
type Backend struct{}

// New returns a macOS Seatbelt backend.
func New() *Backend { return &Backend{} }

// Detect returns a Seatbelt backend when the host can provide the paddock, or
// nil on a host that cannot (advisory tier only). It mirrors
// detect_containment_backend, but lives here rather than in package
// containment because the root module must not import this nested module.
func Detect() containment.Backend {
	b := New()
	if ok, _ := b.Available(); ok {
		return b
	}
	return nil
}

// Name implements containment.Backend.
func (b *Backend) Name() string { return "seatbelt" }

// EnforcementTier implements containment.Backend.
//
// It is "native-sandbox-deny", not "native-syscall-deny" and not
// "native-access-check-deny": Seatbelt is a per-process policy evaluated by
// the sandbox facility, a third shape in the family. The distinct name keeps a
// trace from claiming a mechanism the host did not use.
func (b *Backend) EnforcementTier() string { return "native-sandbox-deny" }

// Available reports whether this host can establish the sandbox, with a
// human-readable reason.
func (b *Backend) Available() (bool, string) { return available() }

// SupportsNetwork reports whether the backend can enforce the given network
// mode.
//
// All three modes are representable. allow_all adds no network rule; deny_all
// adds (deny network-outbound); broker admits only the broker's loopback port
// after that deny. The broker's *host* allowlist is not kernel-pinned — it is
// the broker's decision — but the port pin is, and it is what makes the broker
// the only route out. ResolveSpec rejects a broker policy without a port before
// ProfileFor is reached.
func (b *Backend) SupportsNetwork(mode confinement.NetMode) bool {
	switch mode {
	case confinement.NetAllowAll, confinement.NetDenyAll, confinement.NetBroker, "":
		return true
	default:
		return false
	}
}

// ProfileFor lowers canonical writable roots and the network pin to a
// backend-private profile.
//
// An omitted mode denies egress. A broker policy without a valid port, or any
// unknown mode, is lowered to deny_all rather than to a profile that ignores
// the axis; LaunchConfined refuses those postures before this point, and a
// direct caller gets the fail-closed reading.
func (b *Backend) ProfileFor(roots []string, network confinement.NetworkPolicy) string {
	mode := string(network.Mode)
	port := network.BrokerPort
	switch mode {
	case "":
		mode = modeDenyAll
		port = 0
	case modeAllowAll, modeDenyAll:
		port = 0
	case modeBroker:
		if port <= 0 || port > 65535 {
			mode = modeDenyAll
			port = 0
		}
	default:
		mode = modeDenyAll
		port = 0
	}
	return encodeProfile(profileDoc{
		Roots: canonicalAll(roots),
		Net:   netDoc{Mode: mode, Port: port},
	})
}

// Probe proves the sandbox is BOTH live and grant-conformant before the body
// runs (fail-closed). It is exit-code aware: a sandbox that cannot apply the
// profile is "no paddock", not "write denied", so a broken profile can never
// pass as a working one.
func (b *Backend) Probe(profile, workingRoot string, roots []string) error {
	return probe(profile, workingRoot, roots)
}

// Launch runs a command under the profile.
//
// It does not classify the exit status; a non-zero body exit is returned as-is,
// and the caller maps an authority denial to ErrSandboxDenied when appropriate.
func (b *Backend) Launch(profile string, req containment.LaunchRequest) (containment.LaunchResult, error) {
	return launch(profile, req)
}
