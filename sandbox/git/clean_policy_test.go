package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	trace "github.com/buchenberg/goatherd-kernel"
)

// cpGit runs a git command in dir, failing the test on a non-zero exit.
func cpGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// cpRepo returns an initialized repository with a committed tracked file and a
// gitignore covering ignored/.
func cpRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	// Isolate from the developer's global/system git config (Windows runners
	// default core.autocrlf=true, which rewrites checked-out content to CRLF
	// and breaks byte-exact assertions).
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "gitconfig-system"))
	cpGit(t, repo, "init", "-b", "main")
	cpGit(t, repo, "config", "core.autocrlf", "false")
	cpGit(t, repo, "config", "user.name", "test")
	cpGit(t, repo, "config", "user.email", "test@example.com")
	cpWrite(t, repo, "tracked.txt", "base\n")
	cpWrite(t, repo, ".gitignore", "ignored/\n")
	cpGit(t, repo, "add", "-A")
	cpGit(t, repo, "commit", "-m", "base")
	return repo
}

func cpWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func cpRead(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// cpFindStash locates a retained goatherd-untracked stash dir under the
// repository's git dir.
func cpFindStash(t *testing.T, repo string) string {
	t.Helper()
	dir := filepath.Join(repo, ".git")
	if abs := strings.TrimSpace(cpGit(t, repo, "rev-parse", "--absolute-git-dir")); abs != "" {
		dir = abs
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "goatherd-untracked-") {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

func cpCapture(t *testing.T, sb *GitSandbox) trace.WorkspaceState {
	t.Helper()
	ws, err := sb.Capture(context.Background())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	return ws
}

// The default policy is the historical behavior: a rewind deletes untracked
// content. This is the guarantee that lets an existing host adopt the option
// without changing today's observable results.
func TestApply_DefaultPolicyCleansUntracked(t *testing.T) {
	repo := cpRepo(t)
	sb := NewLocalGitSandbox(repo)
	if got := sb.CleanPolicy(); got != CleanAll {
		t.Errorf("default policy = %v, want %v", got, CleanAll)
	}
	ws := cpCapture(t, sb)

	cpWrite(t, repo, "tracked.txt", "unit edit\n")
	cpWrite(t, repo, "scratch/notes.txt", "doomed\n")

	if err := sb.Apply(context.Background(), ws); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := cpRead(t, repo, "tracked.txt"); got != "base\n" {
		t.Errorf("tracked.txt = %q, want the captured content", got)
	}
	if _, err := os.Lstat(filepath.Join(repo, "scratch", "notes.txt")); !os.IsNotExist(err) {
		t.Errorf("CleanAll must delete untracked content, stat err = %v", err)
	}
	if stash := cpFindStash(t, repo); stash != "" {
		t.Errorf("CleanAll must not create a stash: %s", stash)
	}
}

// The preserve policy's core promise: untracked content the checkpoint does not
// contain survives the rewind with its content intact, while tracked
// modifications are still reverted.
func TestApply_PreserveUntrackedSurvivesApply(t *testing.T) {
	repo := cpRepo(t)
	sb := NewLocalGitSandbox(repo, WithCleanPolicy(CleanPreserveUntracked))
	if got := sb.CleanPolicy(); got != CleanPreserveUntracked {
		t.Errorf("policy = %v, want %v", got, CleanPreserveUntracked)
	}
	ws := cpCapture(t, sb)

	cpWrite(t, repo, "tracked.txt", "unit edit\n")
	cpWrite(t, repo, "internal/demo/roman.go", "package demo\n")
	cpWrite(t, repo, ".agents/roles/demo-scribe.md", "role\n")

	if err := sb.Apply(context.Background(), ws); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := cpRead(t, repo, "tracked.txt"); got != "base\n" {
		t.Errorf("tracked.txt = %q, want the captured content", got)
	}
	if got := cpRead(t, repo, "internal/demo/roman.go"); got != "package demo\n" {
		t.Errorf("post-checkpoint untracked file lost: %q", got)
	}
	if got := cpRead(t, repo, ".agents/roles/demo-scribe.md"); got != "role\n" {
		t.Errorf("post-checkpoint untracked role file lost: %q", got)
	}
	// The stash is drained on success and must not accumulate under .git.
	if stash := cpFindStash(t, repo); stash != "" {
		t.Errorf("stash dir leaked into the git dir: %s", stash)
	}
}

// Untracked content that WAS captured is part of the checkpoint tree and must
// round-trip with its captured content, not be preserved as it stands.
func TestApply_PreserveRestoresCapturedUntracked(t *testing.T) {
	repo := cpRepo(t)
	cpWrite(t, repo, "scratch.txt", "captured\n")
	sb := NewLocalGitSandbox(repo, WithCleanPolicy(CleanPreserveUntracked))
	ws := cpCapture(t, sb)

	cpWrite(t, repo, "scratch.txt", "clobbered\n")
	if err := sb.Apply(context.Background(), ws); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := cpRead(t, repo, "scratch.txt"); got != "captured\n" {
		t.Errorf("captured untracked file = %q, want %q", got, "captured\n")
	}
}

// Empty untracked directories never appear in `git ls-files --others`, so they
// must be preserved by shape or `git clean -fd` still deletes them.
func TestApply_PreserveEmptyDirs(t *testing.T) {
	repo := cpRepo(t)
	sb := NewLocalGitSandbox(repo, WithCleanPolicy(CleanPreserveUntracked))
	ws := cpCapture(t, sb)

	for _, d := range []string{"newdir", "newdir2/emptysub"} {
		if err := os.MkdirAll(filepath.Join(repo, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cpWrite(t, repo, "newdir2/f.txt", "x\n")

	if err := sb.Apply(context.Background(), ws); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, d := range []string{"newdir", "newdir2", "newdir2/emptysub"} {
		fi, err := os.Stat(filepath.Join(repo, filepath.FromSlash(d)))
		if err != nil || !fi.IsDir() {
			t.Errorf("untracked directory %s lost by rewind: err %v", d, err)
		}
	}
	if got := cpRead(t, repo, "newdir2/f.txt"); got != "x\n" {
		t.Errorf("newdir2/f.txt = %q, want %q", got, "x\n")
	}
}

// gitignored content is outside the untracked listing and survives either way:
// the clean never used -x and the preserve path must not either.
func TestApply_PreserveLeavesIgnoredFilesAlone(t *testing.T) {
	repo := cpRepo(t)
	sb := NewLocalGitSandbox(repo, WithCleanPolicy(CleanPreserveUntracked))
	ws := cpCapture(t, sb)

	cpWrite(t, repo, "ignored/data.bin", "precious\n")
	if err := sb.Apply(context.Background(), ws); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := cpRead(t, repo, "ignored/data.bin"); got != "precious\n" {
		t.Errorf("ignored file lost: %q", got)
	}
}

// Lossless-or-loud: when the applied state puts a FILE where a preserved
// untracked DIRECTORY stood, the restore cannot proceed — it must report the
// conflict and keep the only copy of the content in the stash, never delete it
// and report success.
func TestApply_PreserveFileDirConflictIsLoudAndKeepsStash(t *testing.T) {
	repo := cpRepo(t)
	cpWrite(t, repo, "scratch", "i am a file\n")
	cpGit(t, repo, "add", "-A")
	cpGit(t, repo, "commit", "-m", "scratch file")

	sb := NewLocalGitSandbox(repo, WithCleanPolicy(CleanPreserveUntracked))
	ws := cpCapture(t, sb)

	// Replace the tracked file with a directory holding uncaptured content.
	if err := os.Remove(filepath.Join(repo, "scratch")); err != nil {
		t.Fatal(err)
	}
	cpWrite(t, repo, "scratch/notes.txt", "precious notes\n")

	err := sb.Apply(context.Background(), ws)
	if err == nil {
		t.Fatal("a refused restore must be reported, not silently succeed")
	}
	if !strings.Contains(err.Error(), "kept") {
		t.Errorf("error must point at the retained original: %v", err)
	}
	if !strings.Contains(err.Error(), "preserve-untracked") {
		t.Errorf("error must name the active policy: %v", err)
	}
	if got := cpRead(t, repo, "scratch"); got != "i am a file\n" {
		t.Errorf("tracked file not restored: %q", got)
	}
	stash := cpFindStash(t, repo)
	if stash == "" {
		t.Fatal("stash must be retained when a restore is refused")
	}
	if got, rerr := os.ReadFile(filepath.Join(stash, "scratch", "notes.txt")); rerr != nil || string(got) != "precious notes\n" {
		t.Errorf("conflicted original lost from stash: %q err %v", got, rerr)
	}
}

// Lossless-or-loud for the ambiguity that matters most for safety: restoration
// must not follow a symlink the checkpoint reinstated in a parent component, or
// preserved content would be relocated outside the repository.
func TestApply_PreserveSymlinkParentNotFollowed(t *testing.T) {
	repo := cpRepo(t)
	link := filepath.Join(repo, "scratch")
	if err := os.Symlink("../outside", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cpGit(t, repo, "config", "core.symlinks", "true")
	cpGit(t, repo, "add", "-A")
	cpGit(t, repo, "commit", "-m", "scratch symlink")
	// Verify git really stored a symlink; where it degraded to a regular file
	// the scenario under test is not the one present.
	if out := cpGit(t, repo, "ls-files", "-s", "scratch"); !strings.Contains(out, "120000") {
		t.Skip("git did not store a symlink (core.symlinks off)")
	}

	sb := NewLocalGitSandbox(repo, WithCleanPolicy(CleanPreserveUntracked))
	ws := cpCapture(t, sb)

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	cpWrite(t, repo, "scratch/notes.txt", "precious notes\n")
	outside := filepath.Join(repo, "..", "outside")
	if err := os.MkdirAll(outside, 0o755); err == nil {
		t.Cleanup(func() { _ = os.RemoveAll(outside) })
	}

	err := sb.Apply(context.Background(), ws)
	if err == nil {
		t.Fatal("a restore through a symlinked parent must be refused loudly")
	}
	if _, serr := os.Lstat(link); serr != nil {
		t.Errorf("checkpoint symlink not reinstated: %v", serr)
	}
	if _, serr := os.Lstat(filepath.Join(outside, "notes.txt")); serr == nil {
		t.Error("preserved file escaped the repository through the symlinked parent")
	}
	stash := cpFindStash(t, repo)
	if stash == "" {
		t.Fatal("stash must be retained when a restore is refused")
	}
	if got, rerr := os.ReadFile(filepath.Join(stash, "scratch", "notes.txt")); rerr != nil || string(got) != "precious notes\n" {
		t.Errorf("refused original lost from stash: %q err %v", got, rerr)
	}
}

// A failed apply must not strand evacuated content in the stash: the restore
// runs on the error path too.
func TestApply_PreserveRestoresAfterFailedApply(t *testing.T) {
	repo := cpRepo(t)
	sb := NewLocalGitSandbox(repo, WithCleanPolicy(CleanPreserveUntracked))
	ws := cpCapture(t, sb)
	cpWrite(t, repo, "precious.txt", "keep me\n")

	broken := ws
	broken.Data = map[string]any{}
	for k, v := range ws.Data {
		broken.Data[k] = v
	}
	broken.Data["head_sha"] = strings.Repeat("ab", 20) // resolvable-shaped, nonexistent

	if err := sb.Apply(context.Background(), broken); err == nil {
		t.Fatal("expected Apply with a bogus head_sha to fail")
	}
	if got := cpRead(t, repo, "precious.txt"); got != "keep me\n" {
		t.Errorf("evacuated file stranded after failed apply: %q", got)
	}
}

// The policy must not disturb the checkpoint identity contract: capture stays
// content-derived and policy-independent.
func TestApply_PreserveKeepsCaptureStable(t *testing.T) {
	repo := cpRepo(t)
	plain := NewLocalGitSandbox(repo)
	preserving := NewLocalGitSandbox(repo, WithCleanPolicy(CleanPreserveUntracked))

	ws1 := cpCapture(t, plain)
	ws2 := cpCapture(t, preserving)
	d1, err := ws1.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	d2, err := ws2.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if d1 != d2 {
		t.Errorf("clean policy changed the capture digest: %s vs %s", d1, d2)
	}
}
