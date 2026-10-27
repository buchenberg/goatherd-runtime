//go:build linux

package croft

import (
	"context"
	"fmt"
	"time"

	sbx "github.com/buchenberg/goatherd-runtime/sandbox"
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
// # Authority requests are refused, not downgraded
//
// A non-nil Confinement, an explicit paddock Placement, or a non-empty
// AuthorityMatch all ask for enforcement this tier cannot provide: croft's
// container rootfs is wholly writable, so a ConfinementSpec's writable-root
// set has nothing to lower into. Silently satisfying such a request with the
// container boundary would report a grant-shaped authority as enforced when it
// was not.
//
// This is deliberately provisional. Whether the container tier should grow
// shaped authority (read-only rootfs plus explicit writable mounts) or stay a
// coarse boundary that fails loudly is an open design decision tracked as
// goatherd-croft bead croft-ku0; this refusal is the fail-closed placeholder
// until it is settled, and must be revisited then.
func (s *Sandbox) Exec(ctx context.Context, req sbx.ExecRequest) (sbx.ExecResult, error) {
	if err := refuseUnsupportedAuthority(req); err != nil {
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

// refuseUnsupportedAuthority fails closed on an authority request the
// container tier cannot honor. See Exec for why this is a refusal rather than
// a downgrade, and for the croft-ku0 decision that will revisit it.
//
// An advisory or auto placement — and an empty one, meaning "this backend's
// default" — is accepted: neither asks for syscall-tier enforcement, so the
// container boundary is an honest answer to them.
func refuseUnsupportedAuthority(req sbx.ExecRequest) error {
	switch {
	case req.Confinement != nil:
		return fmt.Errorf("croft sandbox: a confinement spec cannot be lowered into a container: the rootfs is wholly writable, so there is no writable-root set to enforce (%w; pending goatherd-croft bead croft-ku0, the container-tier authority vocabulary)", sbx.ErrUnsupported)
	case req.Placement == containment.PlacementPaddock:
		return fmt.Errorf("croft sandbox: placement %q asks for the syscall paddock tier, which this backend does not provide — the container boundary is its enforcement (%w; pending goatherd-croft bead croft-ku0)", req.Placement, sbx.ErrUnsupported)
	case req.AuthorityMatch != "":
		return fmt.Errorf("croft sandbox: an authority-surface digest asserts enforcement this backend cannot evidence (%w; pending goatherd-croft bead croft-ku0)", sbx.ErrUnsupported)
	}
	return nil
}
