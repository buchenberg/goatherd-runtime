// Package containment is the syscall-deny jail contract: the seam that makes an
// authority declaration real at the OS boundary. A Backend lowers a set of
// writable roots (plus the network axis) to an OS-specific profile, probes it
// fail-closed, then launches a command confined by it.
//
// Parity reference: Python Shepherd's vcs-core `_containment.py` (the
// ContainmentBackend protocol and its error taxonomy) and the spec half of
// `_execution_capability.py`. The Linux member (Landlock) lives in the nested
// module .../sandbox/landlock; this package is deliberately stdlib-only so the
// root runtime keeps its "kernel + stdlib" dependency footprint.
package containment

import (
	"errors"
	"time"
)

// Error taxonomy — the failure classes the runtime acts on differently, mirroring
// _containment.py:31-46.
var (
	// ErrSandboxDenied is an out-of-authority operation refused at the syscall
	// (EPERM/EACCES): the write never reached disk. The runtime maps it to a
	// discarded, Failed run — stronger than a check-at-commit gate.
	ErrSandboxDenied = errors.New("containment: operation refused at the syscall")

	// ErrJailNotEstablished is the fail-closed refusal: confinement could not be
	// established, so the run refuses rather than proceeding unconfined.
	ErrJailNotEstablished = errors.New("containment: fail-closed, confinement not established")

	// ErrUnsupportedKernel is the host cannot provide the jail — no LSM, or the
	// syscalls are blocked (e.g. a container seccomp profile denies 444-446).
	ErrUnsupportedKernel = errors.New("containment: kernel does not provide the required LSM (or it is blocked)")

	// ErrUnsupportedConfinementSpec is a spec with no faithful lowering on this
	// host's enforcement surface.
	ErrUnsupportedConfinementSpec = errors.New("containment: spec has no faithful lowering on this host")

	// ErrExecutionAuthorityRequired is a real-execution command dispatched
	// without a capability (version skew / plain prepare): refuse, never a silent
	// unconfined fallback.
	ErrExecutionAuthorityRequired = errors.New("containment: execution command dispatched without execution authority")
)

// LaunchRequest describes one confined execution. WorkingRoot is the containment
// root the probe validates; Cwd is the process working directory (defaulting to
// WorkingRoot). Env, Stdin, and Timeout mirror the caller's execution request.
type LaunchRequest struct {
	Command     []string
	WorkingRoot string
	Cwd         string
	Env         []string
	Stdin       []byte
	Timeout     time.Duration
}

// LaunchResult is the outcome of a confined execution.
type LaunchResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Backend is the syscall-deny jail contract — the Linux member's seam. It lowers
// a writable-root set and the network axis to a profile, probes it fail-closed,
// then launches a command confined by it.
type Backend interface {
	// Name is the backend identifier ("landlock").
	Name() string
	// EnforcementTier is the coarse tier label ("native-syscall-deny").
	EnforcementTier() string
	// Available reports whether the jail can be established on this host, with a
	// human-readable reason.
	Available() (ok bool, why string)
	// ProfileFor lowers a canonical writable-root set (+ the network axis) to a
	// backend-private profile string. allowNetwork is accepted for contract
	// parity; a filesystem-only jail ignores it.
	ProfileFor(roots []string, allowNetwork bool) string
	// Probe proves the profile's jail is BOTH live and grant-conformant before
	// the body runs; it returns ErrJailNotEstablished on any violation.
	Probe(profile, workingRoot string, roots []string) error
	// Launch runs req under the profile and returns its result.
	Launch(profile string, req LaunchRequest) (LaunchResult, error)
}
