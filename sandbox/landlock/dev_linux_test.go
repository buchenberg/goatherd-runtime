//go:build linux

package landlock

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/buchenberg/shepherd-runtime-go/sandbox/containment"
)

func requireSh(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/bin/sh", "/usr/bin/sh"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("no sh(1) available")
	return ""
}

// TestLandlock_DevWritesAllowed pins the _DEV allowance (Seatbelt parity): a
// confined body writes to /dev/null — which shells, `go test`, and build tools
// do constantly — even though /dev is not one of its writable roots. Without the
// allowance, Landlock denies every uncovered path and this write fails.
func TestLandlock_DevWritesAllowed(t *testing.T) {
	b := requireLandlock(t)
	sh := requireSh(t)
	wd := t.TempDir()
	roots := []string{wd}
	prof := b.ProfileFor(roots, allowAll())
	if err := b.Probe(prof, wd, roots); err != nil {
		t.Fatalf("probe: %v", err)
	}

	res, err := b.Launch(prof, containment.LaunchRequest{
		Command:     []string{sh, "-c", "echo ok > /dev/null && echo WROTE"},
		WorkingRoot: wd,
		Cwd:         wd,
		Timeout:     10 * time.Second,
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "WROTE") {
		t.Fatalf("a confined write to /dev/null must be allowed: rc=%d stdout=%q stderr=%q",
			res.ExitCode, res.Stdout, res.Stderr)
	}
}
