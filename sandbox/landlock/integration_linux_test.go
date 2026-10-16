//go:build linux

package landlock

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	sbx "github.com/buchenberg/shepherd-runtime-go/sandbox"
	"github.com/buchenberg/shepherd-runtime-go/sandbox/confinement"
	"github.com/buchenberg/shepherd-runtime-go/sandbox/git"
)

// TestIntegration_GitSandboxPaddockExec exercises the whole stack end to end:
// GitSandbox.Exec → containment.LaunchConfined → landlock backend → trampoline →
// real kernel-enforced paddock. An in-repo write lands; an out-of-repo write is
// refused at the syscall.
func TestIntegration_GitSandboxPaddockExec(t *testing.T) {
	b := requireLandlock(t)
	touch := requireTouch(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")

	sb := git.NewLocalGitSandbox(repo, git.WithExec(b, confinement.PlacementPaddock))
	if !sb.Capabilities().Exec {
		t.Fatal("sandbox with WithExec must report Exec")
	}
	ctx := context.Background()

	inside := filepath.Join(repo, "ok.txt")
	res, err := sb.Exec(ctx, sbx.ExecRequest{Command: touch, Args: []string{inside}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("in-repo write rc=%d stderr=%s", res.ExitCode, res.Stderr)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Fatal("in-repo file should exist")
	}

	outside := filepath.Join(filepath.Dir(repo), "escape.txt")
	_ = os.Remove(outside)
	res, err = sb.Exec(ctx, sbx.ExecRequest{Command: touch, Args: []string{outside}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode == 0 || !res.Denied {
		t.Fatalf("out-of-repo write must be a classified denial, got %+v", res)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("out-of-repo file must not exist")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
