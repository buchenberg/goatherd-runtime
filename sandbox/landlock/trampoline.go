package landlock

// Trampoline sentinels and the confine-failed exit code. Go has no
// `python -m <module>` entry point, so the sentinel is handled by the first
// statement of main() in every binary that spawns confined processes — and by
// the TestMain of each package whose tests exercise Launch — via
// MaybeRunTrampoline.
const (
	// ExecSentinel is argv[1] for a confine-then-exec trampoline invocation.
	ExecSentinel = "__landlock_exec__"
	// ProbeSentinel is argv[1] for a confined write-probe invocation.
	ProbeSentinel = "__landlock_probe__"
	// NetProbeSentinel is argv[1] for a confined TCP-connect-probe invocation.
	NetProbeSentinel = "__landlock_netprobe__"
	// ConfineFailedRC is returned when confinement could not be established — a
	// value an ordinary command (and a denied write) does not produce, so "no
	// paddock" is distinguishable from "write denied".
	ConfineFailedRC = 3
)

// MaybeRunTrampoline handles the hidden trampoline entry point. When args[1] is a
// sentinel it performs the confined action and returns (exitCode, true); the
// caller exits with exitCode. Otherwise it returns (0, false) and the binary
// continues.
//
// The spec is passed in argv (roots JSON + command), never stdin: stdin must
// stay free for the confined command, and environment is passed via the process
// environment so secrets never appear in /proc/<pid>/cmdline. This mirrors the
// Python runner, which passes the profile as one argv element.
func MaybeRunTrampoline(args []string) (int, bool) {
	if len(args) < 2 {
		return 0, false
	}
	switch args[1] {
	case ExecSentinel:
		return runExecTrampoline(args), true
	case ProbeSentinel:
		return runProbeTrampoline(args), true
	case NetProbeSentinel:
		return runNetProbeTrampoline(args), true
	default:
		return 0, false
	}
}
