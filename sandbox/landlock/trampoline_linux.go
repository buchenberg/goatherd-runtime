//go:build linux

package landlock

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// runExecTrampoline confines then replaces the process image.
//
//	argv = [exe, ExecSentinel, rootsJSON, command, args...]
func runExecTrampoline(args []string) int {
	if len(args) < 3 {
		return ConfineFailedRC
	}
	roots := parseRoots(args[2])
	command := args[3:]

	// Pin the OS thread: a Landlock domain is attached to a single task_struct.
	// Without this the goroutine could be rescheduled between restrict_self and
	// Exec, and the jail would silently evaporate onto an unrestricted thread.
	runtime.LockOSThread()
	if err := confine(canonicalAll(roots)); err != nil {
		return ConfineFailedRC
	}
	if len(command) == 0 {
		return 0
	}
	// unix.Exec replaces the process on the same locked thread, so the new image
	// inherits the restrictive domain permanently.
	if err := unix.Exec(command[0], command, os.Environ()); err != nil {
		return 127
	}
	return 127 // unreachable: Exec replaces the process or returns an error
}

// runProbeTrampoline confines then attempts a write to a target path.
//
//	argv = [exe, ProbeSentinel, rootsJSON, targetPath]
//
// Returns 0 when the write succeeded, 1 when it was refused (or otherwise failed
// after confinement), and ConfineFailedRC when confinement itself failed. The
// parent cleans up the canary, so cleanup is unconfined and reliable.
func runProbeTrampoline(args []string) int {
	if len(args) < 4 {
		return ConfineFailedRC
	}
	roots := parseRoots(args[2])
	target := args[3]

	runtime.LockOSThread()
	if err := confine(canonicalAll(roots)); err != nil {
		return ConfineFailedRC
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 1
	}
	_ = f.Close()
	return 0
}
