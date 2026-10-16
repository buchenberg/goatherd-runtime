//go:build linux

package landlock

import (
	"errors"
	"net"
	"os"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// confineSelf pins the OS thread and applies the profile's restrictions. It
// returns false when confinement could not be established, so every trampoline
// verb fails closed. Pinning is load-bearing: a Landlock domain attaches to a
// single task_struct, so the thread that execs must be the one confined.
func confineSelf(profile string) (profileDoc, bool) {
	doc := parseProfile(profile)
	runtime.LockOSThread()
	if err := confine(canonicalAll(doc.Roots), netPolicyOf(doc.Net)); err != nil {
		return profileDoc{}, false
	}
	return doc, true
}

// runExecTrampoline confines then replaces the process image.
//
//	argv = [exe, ExecSentinel, profileJSON, command, args...]
func runExecTrampoline(args []string) int {
	if len(args) < 3 {
		return ConfineFailedRC
	}
	if _, ok := confineSelf(args[2]); !ok {
		return ConfineFailedRC
	}
	command := args[3:]
	if len(command) == 0 {
		return 0
	}
	if err := unix.Exec(command[0], command, os.Environ()); err != nil {
		return 127
	}
	return 127 // unreachable
}

// runProbeTrampoline confines then attempts to create a target file. Returns 0
// on success, 1 when the write is refused (or otherwise fails post-confinement),
// 3 on confine failure.
//
//	argv = [exe, ProbeSentinel, profileJSON, targetPath]
func runProbeTrampoline(args []string) int {
	if len(args) < 4 {
		return ConfineFailedRC
	}
	if _, ok := confineSelf(args[2]); !ok {
		return ConfineFailedRC
	}
	f, err := os.OpenFile(args[3], os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 1
	}
	_ = f.Close()
	return 0
}

// runNetProbeTrampoline confines then attempts a TCP connect. Returns 0 when the
// connect was admitted, 1 when the syscall refused it (the network pin's signal),
// 2 on any other failure (refused-by-peer, timeout), 3 on confine failure.
//
//	argv = [exe, NetProbeSentinel, profileJSON, "host:port"]
func runNetProbeTrampoline(args []string) int {
	if len(args) < 4 {
		return ConfineFailedRC
	}
	if _, ok := confineSelf(args[2]); !ok {
		return ConfineFailedRC
	}
	conn, err := net.DialTimeout("tcp", args[3], 2*time.Second)
	if err == nil {
		_ = conn.Close()
		return 0
	}
	if isSyscallDenied(err) {
		return 1
	}
	return 2
}

// isSyscallDenied reports whether err is a syscall refusal (EACCES/EPERM) — the
// network pin's signal, distinct from an ordinary connect failure.
func isSyscallDenied(err error) bool {
	return errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM)
}
