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

// TestLandlock_DevSubtreeConsequence pins the FORCED trade-off: landlock_add_rule
// needs a directory and Landlock has no deny rules, so granting /dev/null means
// granting /dev — and the world-writable tmpfs at /dev/shm comes with it. This is
// exactly what Seatbelt's `subpath "/dev"` does too; the alternative is denying
// /dev/null outright, which breaks shells and build tools. Pinned so the
// consequence cannot change silently.
func TestLandlock_DevSubtreeConsequence(t *testing.T) {
	b := requireLandlock(t)
	sh := requireSh(t)
	if fi, err := os.Stat("/dev/shm"); err != nil || !fi.IsDir() {
		t.Skip("no /dev/shm on this host")
	}
	wd := t.TempDir()
	roots := []string{wd}
	prof := b.ProfileFor(roots, allowAll())
	if err := b.Probe(prof, wd, roots); err != nil {
		t.Fatalf("probe: %v", err)
	}

	res, err := b.Launch(prof, containment.LaunchRequest{
		Command:     []string{sh, "-c", "echo probe > /dev/shm/yaah-landlock-probe && rm -f /dev/shm/yaah-landlock-probe && echo WROTE"},
		WorkingRoot: wd,
		Cwd:         wd,
		Timeout:     10 * time.Second,
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if !strings.Contains(res.Stdout, "WROTE") {
		t.Fatalf("/dev/shm is granted with /dev — the forced trade-off: rc=%d stdout=%q stderr=%q",
			res.ExitCode, res.Stdout, res.Stderr)
	}
}
