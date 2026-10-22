//go:build windows

package winacl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/buchenberg/goatherd-runtime/sandbox/confinement"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/buchenberg/goatherd-runtime/sandbox/containment"
	"golang.org/x/sys/windows"
)

// available reports whether the host can establish the restriction.
//
// The check is a real capability probe rather than a version comparison: it
// opens the process token with the rights the restriction needs and mints a
// restricted token. A host that cannot do that cannot confine anything, and
// the reason is reported so a caller can surface it.
func available() (bool, string) {
	current, err := openProcessToken()
	if err != nil {
		return false, fmt.Sprintf("cannot open the process token with the rights the restriction needs: %v", err)
	}
	defer current.Close()

	worldSID, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		return false, fmt.Sprintf("cannot build the world SID: %v", err)
	}
	restricted, err := createRestrictedToken(current, nil, worldSID)
	if err != nil {
		return false, fmt.Sprintf("cannot mint a write-restricted token: %v", err)
	}
	restricted.Close()
	return true, "Windows write-restriction (write-restricted token, Low integrity, per-root capability SIDs)"
}

// probe proves the restriction is BOTH live and grant-conformant before the
// body runs.
//
// The three checks are the same ones the Linux member makes, in the same
// order:
//
//  1. liveness — a write outside every writable root must be denied;
//  2. per-root conformance — a canary write beneath each declared writable
//     root must be allowed, catching a profile too strict, which would
//     spuriously fail a legitimate body;
//  3. deny-closed — an in-workdir path outside every root must be denied,
//     catching a mis-lowered broader root.
func probe(profileDoc, workingRoot string, roots []string) error {
	wd := confinement.Canonical(workingRoot)
	canonicalRoots := canonicalAll(roots)
	tag := probeTag()

	// (1) liveness: a write outside every writable root must be denied.
	denied, err := probeWrite(profileDoc, wd, livenessTarget(wd, canonicalRoots, tag))
	if err != nil {
		return err
	}
	if !denied {
		return fmt.Errorf("%w: an out-of-WORKDIR write was NOT denied — no paddock established",
			containment.ErrPaddockNotEstablished)
	}

	// (2) per-root: each declared writable root must accept writes.
	for _, root := range canonicalRoots {
		denied, err := probeWrite(profileDoc, wd, filepath.Join(root, tag+"-canary"))
		if err != nil {
			return err
		}
		if denied {
			return fmt.Errorf("%w: writable root %s DENIES writes (profile too strict) — a legit body's writes would spuriously fail",
				containment.ErrPaddockNotEstablished, root)
		}
	}

	// (3) deny-closed: an in-WORKDIR path outside every writable root must be denied.
	if !withinAnyRoot(wd, canonicalRoots) {
		denied, err := probeWrite(profileDoc, wd, filepath.Join(wd, tag+"-denied"))
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

// livenessTarget returns a path whose parent directory lies outside every
// writable root, so a write to it must be denied.
//
// The parent of the working directory is not always outside the roots — the
// working directory may sit beneath a granted root — so the search walks up
// until it finds a directory no root covers.
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

// probeTimeout bounds one probe invocation.
//
// A probe that hangs is a probe that never concludes, and the probe runs
// before every body — so it is bounded like anything else the backend
// waits on. A timeout fails closed: the runner is killed and the probe
// reports an error, so no body runs on an unproven restriction.
const probeTimeout = 2 * time.Minute

// probeWrite runs a confined write to target and reports whether it was
// denied.
//
// The outcome is read from the exit code, so the probe exercises the real
// enforcement surface rather than the parent's opinion of it. Only two codes
// are meaningful: 0 (the write landed) and 1 (it was denied). The
// confine-failed code means the restriction was never established, and any
// other code is a probe that did not run — both are failures, never a
// denial.
func probeWrite(profileDoc, workingRoot, target string) (bool, error) {
	// Cleanup is parent-side: the parent is unconfined, so it is reliable
	// regardless of the confined outcome.
	_ = os.Remove(target)
	rc, _, stderr, err := invoke(ProbeSentinel, profileDoc, workingRoot, []string{target}, nil, nil, probeTimeout)
	_ = os.Remove(target)
	if err != nil {
		return false, err
	}
	switch rc {
	case 0:
		return false, nil
	case 1:
		return true, nil
	case ConfineFailedRC:
		return false, fmt.Errorf("%w: the Windows restriction could not be established",
			containment.ErrPaddockNotEstablished)
	default:
		return false, fmt.Errorf("winacl: the confinement probe exited with an unexpected code %d (%s)",
			rc, strings.TrimSpace(stderr))
	}
}

// launch runs a command under the profile.
//
// The command is handed to the runner, which applies the grants, mints the
// token, and spawns the child. The exit status is not classified here; the
// caller maps an authority denial to ErrSandboxDenied when appropriate.
func launch(profileDoc string, req containment.LaunchRequest) (containment.LaunchResult, error) {
	if len(req.Command) == 0 {
		return containment.LaunchResult{}, fmt.Errorf("winacl: empty command")
	}
	wd := confinement.Canonical(req.WorkingRoot)
	cwd := wd
	if req.Cwd != "" {
		cwd = confinement.Canonical(req.Cwd)
	}
	rc, stdout, stderr, err := invoke(RunnerSentinel, profileDoc, cwd, req.Command, req.Env, req.Stdin, req.Timeout)
	if err != nil {
		return containment.LaunchResult{}, err
	}
	return containment.LaunchResult{ExitCode: rc, Stdout: stdout, Stderr: stderr}, nil
}

// invoke runs the runner in this binary with argv = [sentinel, profile,
// extra...]. It returns the exit code and the captured output. A non-nil
// error means the runner itself could not be started.
//
// The environment and the standard input are the runner's own, which is how
// they reach the confined child: the runner builds the child's environment
// block from its own, and the child's stdin is the runner's stdin. A nil
// environment means the runner inherits its own, which is the caller's.
func invoke(sentinel, profileDoc, dir string, extra []string, env []string, stdin []byte, timeout time.Duration) (int, string, string, error) {
	executable, err := os.Executable()
	if err != nil {
		return -1, "", "", fmt.Errorf("winacl: locate own executable: %w", err)
	}
	argv := make([]string, 0, len(extra)+2)
	argv = append(argv, sentinel, profileDoc)
	argv = append(argv, extra...)

	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, executable, argv...)
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

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), stdout.String(), stderr.String(), nil
		}
		return -1, stdout.String(), stderr.String(), fmt.Errorf("winacl: run the confinement runner: %w", err)
	}
	return 0, stdout.String(), stderr.String(), nil
}

// probeSeq is a process-wide counter so a probe's canary names are unique
// and never collide with — or delete — a real workspace file.
var probeSeq atomic.Uint64

// probeTag returns a per-probe unique name.
func probeTag() string {
	return fmt.Sprintf(".winacl-probe-%d-%d", os.Getpid(), probeSeq.Add(1))
}
