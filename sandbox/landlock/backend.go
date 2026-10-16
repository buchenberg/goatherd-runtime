// Package landlock is the Linux Landlock containment backend — the native
// syscall-deny paddock tier with no container and no root. It is the Go port of
// vcs-core/_landlock_containment.py:LandlockContainmentBackend.
//
// The backend compiles a deny-closed profile (a JSON array of canonical writable
// roots), proves the paddock is both live and grant-conformant with an
// exit-code-aware probe, and launches commands through a confine-then-exec
// trampoline in this same binary (see MaybeRunTrampoline).
//
// This is a nested module so the root runtime keeps its "kernel + stdlib"
// dependency footprint: only this module pulls golang.org/x/sys directly.
package landlock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/buchenberg/shepherd-runtime-go/sandbox/containment"
)

// Backend is the Linux Landlock containment backend.
type Backend struct{}

// New returns a Landlock backend.
func New() *Backend { return &Backend{} }

// Detect returns a Landlock backend when the host can provide the paddock, or nil on
// a paddock-less host (advisory tier only). It mirrors detect_containment_backend,
// but lives here rather than in package containment because the root module must
// not import this nested module.
func Detect() containment.Backend {
	b := New()
	if ok, _ := b.Available(); ok {
		return b
	}
	return nil
}

// Name implements containment.Backend.
func (b *Backend) Name() string { return "landlock" }

// EnforcementTier implements containment.Backend.
func (b *Backend) EnforcementTier() string { return "native-syscall-deny" }

// Available probes the kernel Landlock ABI.
func (b *Backend) Available() (bool, string) {
	version, err := abi()
	if err != nil {
		return false, err.Error()
	}
	return true, fmt.Sprintf("Landlock ABI %d", version)
}

// ProfileFor lowers a canonical writable-root set to a backend-private profile:
// a JSON array of the roots (empty array = ReadOnly). allowNetwork is accepted
// for contract parity and ignored — Landlock governs filesystem writes only.
func (b *Backend) ProfileFor(roots []string, allowNetwork bool) string {
	_ = allowNetwork
	payload, err := json.Marshal(canonicalAll(roots))
	if err != nil {
		return "[]"
	}
	return string(payload)
}

// Probe proves the paddock is BOTH live and grant-conformant before the body runs
// (fail-closed §6). It is exit-code aware: a confine failure (rc 3) is "no paddock",
// not "write denied", so a broken Landlock can never pass as a working paddock.
//
// Mirrors _landlock_containment.py:probe (three checks: liveness, per-root
// conformance, deny-closed).
func (b *Backend) Probe(profile, workingRoot string, roots []string) error {
	wd := containment.Canonical(workingRoot)
	canonicalRoots := canonicalAll(roots)

	// (1) liveness: an out-of-WORKDIR write must be denied.
	denied, err := b.probeDenied(profile, wd, filepath.Join(filepath.Dir(wd), ".paddock-probe"))
	if err != nil {
		return err
	}
	if !denied {
		return fmt.Errorf("%w: out-of-WORKDIR write was NOT denied — no paddock established", containment.ErrPaddockNotEstablished)
	}

	// (2) per-root: each declared writable root must accept writes.
	for _, root := range canonicalRoots {
		denied, err := b.probeDenied(profile, wd, filepath.Join(root, ".paddock-probe-canary"))
		if err != nil {
			return err
		}
		if denied {
			return fmt.Errorf("%w: writable root %s DENIES writes (profile too strict) — a legit body's writes would spuriously fail",
				containment.ErrPaddockNotEstablished, root)
		}
	}

	// (3) deny-closed: an in-WORKDIR path outside every writable root must be
	// denied, catching a ReadOnly run mis-lowered to a writable WORKDIR.
	if !withinAnyRoot(wd, canonicalRoots) {
		denied, err := b.probeDenied(profile, wd, filepath.Join(wd, ".paddock-probe-denied"))
		if err != nil {
			return err
		}
		if !denied {
			return fmt.Errorf("%w: an in-WORKDIR path outside every writable root was PERMITTED — would silently escalate",
				containment.ErrPaddockNotEstablished)
		}
	}
	return nil
}

// probeDenied reports whether a confined write to target was denied. Cleanup is
// parent-side (the parent is unconfined), so it is reliable regardless of the
// confined outcome.
func (b *Backend) probeDenied(profile, workingRoot, target string) (bool, error) {
	_ = os.Remove(target)
	rc, _, _, err := b.invoke(ProbeSentinel, profile, workingRoot, []string{target}, nil, nil, 0)
	_ = os.Remove(target)
	if err != nil {
		return false, err
	}
	if rc == ConfineFailedRC {
		return false, fmt.Errorf("%w: Landlock confinement could not be established", containment.ErrPaddockNotEstablished)
	}
	return rc != 0, nil
}

// Launch runs req under the profile. It does not classify the exit status; a
// non-zero body exit is returned as-is (the caller maps an authority denial to
// ErrSandboxDenied when appropriate).
func (b *Backend) Launch(profile string, req containment.LaunchRequest) (containment.LaunchResult, error) {
	if len(req.Command) == 0 {
		return containment.LaunchResult{}, fmt.Errorf("landlock: empty command")
	}
	wd := containment.Canonical(req.WorkingRoot)
	cwd := wd
	if req.Cwd != "" {
		cwd = containment.Canonical(req.Cwd)
	}
	rc, stdout, stderr, err := b.invoke(ExecSentinel, profile, cwd, req.Command, req.Env, req.Stdin, req.Timeout)
	if err != nil {
		return containment.LaunchResult{}, err
	}
	return containment.LaunchResult{ExitCode: rc, Stdout: stdout, Stderr: stderr}, nil
}

// invoke runs the trampoline in this binary with argv = [sentinel, profile, extra...].
// It returns the process exit code and captured output. A non-nil error means the
// trampoline itself could not be started.
func (b *Backend) invoke(sentinel, profile, dir string, extra, env []string, stdin []byte, timeout time.Duration) (int, string, string, error) {
	exe, err := os.Executable()
	if err != nil {
		return -1, "", "", fmt.Errorf("landlock: locate own executable: %w", err)
	}
	argv := make([]string, 0, len(extra)+2)
	argv = append(argv, sentinel, profile)
	argv = append(argv, extra...)

	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, exe, argv...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return exitErr.ExitCode(), stdout.String(), stderr.String(), nil
		}
		return -1, stdout.String(), stderr.String(), fmt.Errorf("landlock: run trampoline: %w", runErr)
	}
	return 0, stdout.String(), stderr.String(), nil
}

// withinAnyRoot reports whether p is one of roots or lies beneath one.
func withinAnyRoot(p string, roots []string) bool {
	for _, root := range roots {
		if p == root || withinOrEqual(p, root) {
			return true
		}
	}
	return false
}

func withinOrEqual(child, parent string) bool {
	if parent == "" {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
