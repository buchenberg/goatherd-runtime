//go:build linux

package croft

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	sbx "github.com/buchenberg/goatherd-runtime/sandbox"
	"github.com/buchenberg/goatherd-runtime/sandbox/confinement"
	"github.com/buchenberg/goatherd-runtime/sandbox/containment"

	croftlib "github.com/buchenberg/goatherd-croft"
)

// execCeiling bounds a single Exec whose request carries no Timeout, so a
// caller passing context.Background() cannot hang the session on a wedged
// daemon. It matches the git and landlock backends' 10-minute default, and is
// a var so tests can shrink it instead of sleeping.
//
// An explicit request Timeout above the ceiling is honored as the caller's
// contract, not clamped — the same rule the Sandbox interface documents.
var execCeiling = 10 * time.Minute

// Exec runs a command inside the container.
//
// The container is the boundary: the command runs in the container's own
// namespaces and cgroups, under the network posture Config.Network selected.
// Nothing is enforced at the syscall level, so the result never carries
// Denied or AuthorityEvidence — both are meaningful only for a paddocked run,
// and reporting them here would claim enforcement this backend does not do.
//
// The run is always bounded: the request's Timeout when it carries one,
// otherwise execCeiling. A run the deadline kills surfaces as a
// context-deadline error rather than a result with a negative exit code.
//
// # Authority requests
//
// The shaping is fixed at construction: Config.WritableRoots and Config.Network
// determine the container's mounts and egress before the task starts. A
// ConfinementSpec on Exec is therefore checked against that configured shape
// rather than silently ignored. A matching spec is accepted; a mismatched one
// is refused. An empty writable-root set is not enforceable as read-only
// because empty Config.WritableRoots means the historical unshaped workspace,
// so a read-only request is refused rather than downgraded.
func (s *Sandbox) Exec(ctx context.Context, req sbx.ExecRequest) (sbx.ExecResult, error) {
	if err := checkAuthority(req, s.cfg); err != nil {
		return sbx.ExecResult{}, err
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = execCeiling
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, err := s.inner.Exec(ctx, croftlib.ExecRequest{
		Command: req.Command,
		Args:    req.Args,
		Cwd:     req.Cwd,
		Env:     req.Env,
		Timeout: timeout,
		Stdin:   req.Stdin,
	})
	if err != nil {
		return sbx.ExecResult{}, fmt.Errorf("croft sandbox: exec: %w", err)
	}
	return sbx.ExecResult{
		ExitCode: res.ExitCode,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		Duration: res.Duration,
	}, nil
}

// checkAuthority rejects requests the configured container shape cannot honor.
// It accepts a matching ConfinementSpec, because Config already lowered that
// declaration into mounts/network; it refuses a mismatch rather than pretending
// the running container changed shape.
func checkAuthority(req sbx.ExecRequest, cfg Config) error {
	switch req.Placement {
	case "", containment.PlacementAdvisory, containment.PlacementAuto:
		// No syscall-tier enforcement requested; the container boundary is an
		// honest answer.
	case containment.PlacementPaddock:
		return fmt.Errorf("croft sandbox: placement %q asks for the syscall paddock tier, which this backend does not provide — the container boundary is its enforcement (%w)", req.Placement, sbx.ErrUnsupported)
	default:
		return fmt.Errorf("croft sandbox: unknown placement %q (valid: %q, %q, %q, or empty for this backend's default)",
			req.Placement, containment.PlacementPaddock, containment.PlacementAdvisory, containment.PlacementAuto)
	}
	if req.AuthorityMatch != "" {
		return fmt.Errorf("croft sandbox: an authority-surface digest asserts enforcement this backend cannot evidence (%w)", sbx.ErrUnsupported)
	}
	if req.Confinement == nil {
		return nil
	}
	roots, network, err := confinement.ResolveSpec(*req.Confinement)
	if err != nil {
		return fmt.Errorf("croft sandbox: resolve confinement: %w", err)
	}
	if len(roots) == 0 {
		// Config.WritableRoots == nil is the historical unshaped workspace,
		// not an all-read-only shape. A read-only ConfinementSpec therefore
		// cannot be honored without a separate shaped flag.
		return fmt.Errorf("croft sandbox: a read-only confinement request cannot be lowered into an unshaped container workspace (%w)", sbx.ErrUnsupported)
	}
	if !sameRoots(roots, cfg.WritableRoots) {
		return fmt.Errorf("croft sandbox: requested writable roots %v do not match the configured container shape %v (%w)", roots, cfg.WritableRoots, sbx.ErrUnsupported)
	}
	if !sameNetwork(network, cfg) {
		return fmt.Errorf("croft sandbox: requested network %q does not match the configured container posture %q (%w)", network.Mode, cfg.Network, sbx.ErrUnsupported)
	}
	if !sameHosts(network.AllowedHosts, cfg.BrokerHosts) {
		return fmt.Errorf("croft sandbox: requested broker hosts %v do not match the configured allowlist %v (%w)", network.AllowedHosts, cfg.BrokerHosts, sbx.ErrUnsupported)
	}
	return nil
}

func sameRoots(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	a := make([]string, len(got))
	b := make([]string, len(want))
	for i, p := range got {
		a[i] = filepath.Clean(p)
	}
	for i, p := range want {
		b[i] = filepath.Clean(p)
	}
	sort.Strings(a)
	sort.Strings(b)
	return slices.Equal(a, b)
}

func sameNetwork(network confinement.NetworkPolicy, cfg Config) bool {
	switch network.Mode {
	case confinement.NetAllowAll:
		return cfg.Network == "host"
	case confinement.NetDenyAll:
		return cfg.Network == "" || cfg.Network == "none"
	case confinement.NetBroker:
		return cfg.Network == "broker"
	default:
		return false
	}
}

func sameHosts(got, want []string) bool {
	a := make([]string, len(got))
	b := make([]string, len(want))
	for i, h := range got {
		a[i] = strings.ToLower(strings.TrimSpace(h))
	}
	for i, h := range want {
		b[i] = strings.ToLower(strings.TrimSpace(h))
	}
	sort.Strings(a)
	sort.Strings(b)
	return slices.Equal(a, b)
}
