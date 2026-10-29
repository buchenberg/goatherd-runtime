//go:build darwin

package seatbelt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/buchenberg/goatherd-runtime/sandbox/confinement"
	"github.com/buchenberg/goatherd-runtime/sandbox/containment"
)

// sandboxExecPath is Apple's Seatbelt launcher. It is deprecated but still
// present on macOS; Available reports its absence rather than assuming it.
const sandboxExecPath = "/usr/bin/sandbox-exec"

// execCeiling bounds a launch whose request carries no timeout, mirroring the
// Landlock and git backends. An explicit request Timeout is honored as given.
var execCeiling = 10 * time.Minute

// probeTimeout bounds one probe invocation. The probe runs before every body,
// so a probe that hangs must fail closed rather than wait forever.
const probeTimeout = 2 * time.Minute

// available reports whether the host can establish the sandbox.
func available() (bool, string) {
	info, err := os.Stat(sandboxExecPath)
	if err != nil {
		return false, fmt.Sprintf("%s not found", sandboxExecPath)
	}
	if info.Mode()&0o111 == 0 {
		return false, fmt.Sprintf("%s is not executable", sandboxExecPath)
	}
	return true, "sandbox-exec present"
}

// probe proves the sandbox is BOTH live and grant-conformant before the body
// runs.
//
// The checks are the Landlock member's, plus a Seatbelt-specific warm-up and
// network pin:
//
//  0. warm-up — a trivial command must run under the rendered profile, proving
//     sandbox-exec accepted it and can spawn a child;
//  1. liveness — a write outside every writable root must be denied;
//  2. per-root — a canary write beneath each declared root must be allowed,
//     catching a profile too strict for a legitimate body;
//  3. deny-closed — an in-workdir path outside every root must be denied,
//     catching a mis-lowered broader root;
//  4. network — deny_all refuses a loopback connect; broker refuses a
//     non-pinned port and admits the pinned one.
func probe(profile, workingRoot string, roots []string) error {
	doc, err := parseProfile(profile)
	if err != nil {
		return fmt.Errorf("%w: %v", containment.ErrPaddockNotEstablished, err)
	}
	if err := doc.validate(); err != nil {
		return fmt.Errorf("%w: %v", containment.ErrPaddockNotEstablished, err)
	}
	sbpl := renderSBPL(doc)
	wd := confinement.Canonical(workingRoot)
	canonicalRoots := canonicalAll(roots)
	tag := probeTag()

	// (0) warm-up: prove sandbox-exec applied the profile at all. Without
	// this, a ReadOnly profile whose sandbox never started could pass the
	// write checks vacuously: every target would simply be absent.
	rc, _, stderr, err := runSandboxExec(sbpl, wd, []string{"/usr/bin/true"}, nil, nil, probeTimeout)
	if err != nil {
		return fmt.Errorf("%w: %v", containment.ErrPaddockNotEstablished, err)
	}
	if rc != 0 {
		return fmt.Errorf("%w: the Seatbelt profile could not be applied (sandbox-exec exited %d: %s)",
			containment.ErrPaddockNotEstablished, rc, strings.TrimSpace(stderr))
	}

	// (1) liveness: a write outside every writable root must be denied.
	allowed, err := probeWrite(sbpl, wd, livenessTarget(wd, canonicalRoots, tag))
	if err != nil {
		return err
	}
	if allowed {
		return fmt.Errorf("%w: an out-of-WORKDIR write was NOT denied — no sandbox established",
			containment.ErrPaddockNotEstablished)
	}

	// (2) per-root: each declared writable root must accept writes.
	for _, root := range canonicalRoots {
		allowed, err := probeWrite(sbpl, wd, filepath.Join(root, tag+"-canary"))
		if err != nil {
			return err
		}
		if !allowed {
			return fmt.Errorf("%w: writable root %s DENIES writes (profile too strict) — a legit body's writes would spuriously fail",
				containment.ErrPaddockNotEstablished, root)
		}
	}

	// (3) deny-closed: an in-WORKDIR path outside every writable root must be
	// denied, catching a ReadOnly run mis-lowered to a writable WORKDIR.
	if !withinAnyRoot(wd, canonicalRoots) {
		allowed, err := probeWrite(sbpl, wd, filepath.Join(wd, tag+"-denied"))
		if err != nil {
			return err
		}
		if allowed {
			return fmt.Errorf("%w: an in-WORKDIR path outside every writable root was PERMITTED — would silently escalate",
				containment.ErrPaddockNotEstablished)
		}
	}

	return probeNetwork(sbpl, doc, wd)
}

// livenessTarget returns a path whose parent directory lies outside every
// writable root. The working directory may itself sit beneath a granted root,
// so the search walks up until it finds a directory no root covers.
func livenessTarget(wd string, roots []string, tag string) string {
	dir := filepath.Dir(wd)
	for withinAnyRoot(dir, roots) {
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return filepath.Join(dir, tag+"-liveness")
}

// probeWrite runs a confined write to target and reports whether it landed.
//
// The outcome is read from the filesystem, exactly as the reference does: a
// successful create is the allow signal, and a missing file with exit code 1
// is the denial signal. A missing file with any other exit code means the
// probe did not run — a broken sandbox — and is a fail-closed error, never a
// denial. Cleanup is parent-side (the parent is unconfined).
func probeWrite(sbpl, workingRoot, target string) (bool, error) {
	_ = os.Remove(target)
	rc, _, stderr, err := runSandboxExec(sbpl, workingRoot, []string{"/usr/bin/touch", target}, nil, nil, probeTimeout)
	_, statErr := os.Stat(target)
	_ = os.Remove(target)
	if err != nil {
		return false, fmt.Errorf("%w: %v", containment.ErrPaddockNotEstablished, err)
	}
	if statErr == nil {
		return true, nil
	}
	switch rc {
	case 1:
		return false, nil
	case 0:
		return false, fmt.Errorf("%w: the write probe exited 0 but created nothing", containment.ErrPaddockNotEstablished)
	default:
		return false, fmt.Errorf("%w: the write probe exited with an unexpected code %d (%s)",
			containment.ErrPaddockNotEstablished, rc, strings.TrimSpace(stderr))
	}
}

// probeNetwork validates the profile's network pin fail-closed.
func probeNetwork(sbpl string, doc profileDoc, workingRoot string) error {
	switch doc.Net.Mode {
	case modeAllowAll:
		return nil
	case modeDenyAll:
		port, closeListener, err := listenLoopback()
		if err != nil {
			return fmt.Errorf("%w: cannot start a loopback probe listener: %v",
				containment.ErrPaddockNotEstablished, err)
		}
		defer closeListener()
		allowed, err := netProbe(sbpl, workingRoot, port)
		if err != nil {
			return err
		}
		if allowed {
			return fmt.Errorf("%w: the deny_all network pin did NOT deny a loopback connect",
				containment.ErrPaddockNotEstablished)
		}
		return nil
	case modeBroker:
		// A non-pinned port must be refused. Pick a port that is not the pin:
		// an ephemeral allocation that happened to land on the pinned port
		// would make the allowed port look like a non-pinned violation.
		port, closeListener, err := listenLoopbackOther(doc.Net.Port)
		if err != nil {
			return fmt.Errorf("%w: cannot start a loopback probe listener: %v",
				containment.ErrPaddockNotEstablished, err)
		}
		allowed, err := netProbe(sbpl, workingRoot, port)
		closeListener()
		if err != nil {
			return err
		}
		if allowed {
			return fmt.Errorf("%w: a non-pinned egress port was PERMITTED — the network pin is not deny-closed",
				containment.ErrPaddockNotEstablished)
		}

		// The pinned port must be admitted. Bind it when it is free; when it is
		// already held, assume the broker is listening and connect to it.
		pinned := doc.Net.Port
		if ln, err := net.Listen("tcp", loopbackAddr(pinned)); err == nil {
			defer ln.Close()
		}
		allowed, err = netProbe(sbpl, workingRoot, pinned)
		if err != nil {
			return err
		}
		if !allowed {
			return fmt.Errorf("%w: the pinned egress port %d DENIES connects (is the broker listening?)",
				containment.ErrPaddockNotEstablished, pinned)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown network mode %q", containment.ErrPaddockNotEstablished, doc.Net.Mode)
	}
}

// netProbe reports whether a confined TCP connect to 127.0.0.1:port landed.
func netProbe(sbpl, workingRoot string, port int) (bool, error) {
	rc, _, stderr, err := runSandboxExec(sbpl, workingRoot,
		[]string{"/usr/bin/nc", "-z", "-w", "1", "127.0.0.1", strconv.Itoa(port)},
		nil, nil, probeTimeout)
	if err != nil {
		return false, fmt.Errorf("%w: %v", containment.ErrPaddockNotEstablished, err)
	}
	switch rc {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("%w: the network probe exited with an unexpected code %d (%s)",
			containment.ErrPaddockNotEstablished, rc, strings.TrimSpace(stderr))
	}
}

// listenLoopback starts a listener on an ephemeral loopback port and returns
// the port and its close function. A connect completes against the listen
// backlog without an Accept, which is all the probe needs.
func listenLoopback() (int, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, func() {}, err
	}
	return ln.Addr().(*net.TCPAddr).Port, func() { _ = ln.Close() }, nil
}

// listenLoopbackOther starts a listener on an ephemeral loopback port that is
// not avoid.
func listenLoopbackOther(avoid int) (int, func(), error) {
	for attempt := 0; attempt < 8; attempt++ {
		port, closeListener, err := listenLoopback()
		if err != nil {
			return 0, func() {}, err
		}
		if port != avoid {
			return port, closeListener, nil
		}
		closeListener()
	}
	return 0, func() {}, fmt.Errorf("could not allocate a loopback port distinct from %d", avoid)
}

func loopbackAddr(port int) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// launch runs a command under the profile.
//
// The command is handed to sandbox-exec directly: Seatbelt applies the profile
// to the child it spawns, so no self-reexec trampoline is needed (Landlock and
// winacl need one because they confine in-process). The rendered profile is
// re-validated first so a malformed document fails closed rather than reaching
// sandbox-exec.
func launch(profile string, req containment.LaunchRequest) (containment.LaunchResult, error) {
	if len(req.Command) == 0 {
		return containment.LaunchResult{}, fmt.Errorf("seatbelt: empty command")
	}
	doc, err := parseProfile(profile)
	if err != nil {
		return containment.LaunchResult{}, fmt.Errorf("%w: %v", containment.ErrPaddockNotEstablished, err)
	}
	if err := doc.validate(); err != nil {
		return containment.LaunchResult{}, fmt.Errorf("%w: %v", containment.ErrPaddockNotEstablished, err)
	}
	wd := confinement.Canonical(req.WorkingRoot)
	cwd := wd
	if req.Cwd != "" {
		cwd = confinement.Canonical(req.Cwd)
	}
	rc, stdout, stderr, err := runSandboxExec(renderSBPL(doc), cwd, req.Command, req.Env, req.Stdin, req.Timeout)
	if err != nil {
		return containment.LaunchResult{}, err
	}
	return containment.LaunchResult{ExitCode: rc, Stdout: stdout, Stderr: stderr}, nil
}

// runSandboxExec runs command under sbpl with sandbox-exec and returns its exit
// code and captured output. A non-nil error means sandbox-exec itself could not
// be started or the deadline expired; a non-zero exit code from the command is
// returned as a result, not an error.
func runSandboxExec(sbpl, dir string, command, env []string, stdin []byte, timeout time.Duration) (int, string, string, error) {
	ceiling := timeout
	if ceiling <= 0 {
		ceiling = execCeiling
	}
	ctx, cancel := context.WithTimeout(context.Background(), ceiling)
	defer cancel()

	argv := make([]string, 0, len(command)+2)
	argv = append(argv, "-p", sbpl)
	argv = append(argv, command...)

	cmd := exec.CommandContext(ctx, sandboxExecPath, argv...)
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
		if ctxErr := ctx.Err(); ctxErr != nil {
			return -1, stdout.String(), stderr.String(), fmt.Errorf("seatbelt: run sandbox-exec: %w", ctxErr)
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return exitErr.ExitCode(), stdout.String(), stderr.String(), nil
		}
		return -1, stdout.String(), stderr.String(), fmt.Errorf("seatbelt: run sandbox-exec: %w", runErr)
	}
	return 0, stdout.String(), stderr.String(), nil
}

// probeSeq is a process-wide counter so probe canary names are unique and
// never collide with — or delete — a real workspace file.
var probeSeq atomic.Uint64

func probeTag() string {
	return fmt.Sprintf(".seatbelt-probe-%d-%d", os.Getpid(), probeSeq.Add(1))
}
