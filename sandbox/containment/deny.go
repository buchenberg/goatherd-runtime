package containment

import (
	"fmt"
	"strings"
)

// DeniedExit heuristically classifies a confined run's result as a syscall denial
// rather than an ordinary command failure.
//
// A Landlock denial is transparent — the kernel returns EACCES/EPERM to the
// confined process, which reports it in its own diagnostic — so the only
// portable signal is stderr. The Python reference defines SandboxDenied as
// vocabulary without ever raising it; this classifier is what makes the §9.4
// mapping ("denied → discarded, Failed run") real in Go.
//
// A command that exits non-zero for unrelated reasons but mentions permission in
// its stderr can be misclassified; callers needing exactness should use a
// command with a known exit code. The fail-closed probe, not this heuristic, is
// the authority for whether a paddock is live.
func DeniedExit(res LaunchResult) bool {
	if res.ExitCode == 0 {
		return false
	}
	stderr := strings.ToLower(res.Stderr)
	return strings.Contains(stderr, "permission denied") ||
		strings.Contains(stderr, "operation not permitted")
}

// DenyError returns an ErrSandboxDenied-wrapped error when res reads as a denial,
// else nil. It is the mapping the runtime uses to record a discarded, Failed run:
// the write never reached disk, which is stronger than a check-at-commit gate.
func DenyError(res LaunchResult, context string) error {
	if !DeniedExit(res) {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrSandboxDenied, context)
}
