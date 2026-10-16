package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGitSandbox_FileIO(t *testing.T) {
	repo := newTestRepo(t)
	sb := NewLocalGitSandbox(repo)
	ctx := context.Background()

	// WriteFile creates parent directories and lands the file in the workspace.
	if err := sb.WriteFile(ctx, "nested/dir/hello.txt", []byte("hi"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "nested", "dir", "hello.txt")); err != nil {
		t.Fatalf("written file missing: %v", err)
	}

	got, err := sb.ReadFile(ctx, "nested/dir/hello.txt")
	if err != nil || string(got) != "hi" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	// Absolute in-workspace paths are accepted.
	if _, err := sb.ReadFile(ctx, filepath.Join(repo, "nested", "dir", "hello.txt")); err != nil {
		t.Fatalf("absolute ReadFile: %v", err)
	}
}

func TestGitSandbox_FileIO_EscapeRefused(t *testing.T) {
	repo := newTestRepo(t)
	sb := NewLocalGitSandbox(repo)
	ctx := context.Background()

	if _, err := sb.ReadFile(ctx, "../outside.txt"); err == nil {
		t.Fatal("a `..` read must be refused")
	}
	if err := sb.WriteFile(ctx, "../outside.txt", []byte("x"), 0o644); err == nil {
		t.Fatal("a `..` write must be refused")
	}
}

func TestGitSandbox_FileIO_SymlinkEscapeRefused(t *testing.T) {
	repo := newTestRepo(t)
	sb := NewLocalGitSandbox(repo)
	ctx := context.Background()

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(repo, "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := sb.ReadFile(ctx, "link.txt"); err == nil {
		t.Fatal("reading through a symlink that escapes the workspace must be refused")
	}
}

func TestGitSandbox_FileIO_SymlinkMkdirEscapeRefused(t *testing.T) {
	repo := newTestRepo(t)
	sb := NewLocalGitSandbox(repo)
	ctx := context.Background()

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(repo, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	// A write whose parent would be created *through* an escaping symlink must be
	// refused, and must not create the directory outside the workspace.
	if err := sb.WriteFile(ctx, "link/new/x.txt", []byte("x"), 0o644); err == nil {
		t.Fatal("a write through an escaping symlinked intermediate must be refused")
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
		t.Fatalf("mkdir escaped the workspace: %s/new exists", outside)
	}
}
