package git

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// CleanPolicy selects what the apply (rewind) sequence does with untracked
// content in the workspace.
//
// The apply sequence has always run `git clean -fd`, which deletes every
// untracked path — including content created after the checkpoint was captured:
// orchestrator scratch files, freshly authored role definitions, empty scratch
// directories, sub-agent output directories. For a workspace whose owner treats
// the tree as a live host workspace rather than a disposable checkout, that is
// losing data the rewind was never meant to reclaim.
//
// The policy does not change the git commands Apply runs; it decides what those
// commands are allowed to see first. Under CleanPreserveUntracked the untracked
// content absent from the target state is evacuated into a stash directory
// under the git dir before the clean, and moved back once the apply has
// finished, so `git clean -fd` reclaims exactly what the captured state holds
// and nothing else.
type CleanPolicy int

const (
	// CleanAll is the default and the historical behavior: `git clean -fd`
	// runs over the full worktree, so untracked content is deleted by a
	// rewind. Choosing it keeps Apply byte-identical to a sandbox built
	// without WithCleanPolicy.
	CleanAll CleanPolicy = iota

	// CleanPreserveUntracked keeps untracked content the target state does
	// not contain. Preservation is lossless-or-loud: content that cannot be
	// moved back — because the applied state put something else at its
	// path, or because a parent component is no longer a plain directory —
	// stays in the stash under the git dir and Apply reports both the stash
	// location and the reason. Nothing is ever destroyed silently.
	CleanPreserveUntracked
)

// String renders the policy for logs and error messages. An unrecognized value
// renders as the default: only CleanPreserveUntracked opts into the new
// behavior, so a misconfigured policy fails safe onto the byte-identical path.
func (p CleanPolicy) String() string {
	if p == CleanPreserveUntracked {
		return "preserve-untracked"
	}
	return "clean-all"
}

// WithCleanPolicy sets the apply-time clean policy. Without this option the
// sandbox uses CleanAll, so existing hosts are unaffected.
func WithCleanPolicy(p CleanPolicy) Option {
	return func(g *GitSandbox) {
		g.cleanPolicy = p
	}
}

// CleanPolicy reports the policy Apply uses with this sandbox. A host that
// adopts preserve semantics can surface the resolved policy in its own
// diagnostics; the zero value is CleanAll.
func (g *GitSandbox) CleanPolicy() CleanPolicy { return g.cleanPolicy }

// stashedFile records one evacuated path and where its content waits.
type stashedFile struct {
	rel     string // workdir-relative path, slash-separated (git's form)
	stashed string // absolute path inside the stash dir
}

// untrackedStash is everything an apply moved aside and must put back: file
// contents in the stash dir, plus directory shapes to recreate. Empty untracked
// directories carry no content, so recording their paths is enough — `git clean
// -fd` removes them and the restore re-creates them.
type untrackedStash struct {
	dir   string
	files []stashedFile
	dirs  []string // workdir-relative, slash-separated
}

func (s untrackedStash) empty() bool { return len(s.files) == 0 && len(s.dirs) == 0 }

// evacuateUntracked moves aside the untracked content that the target state
// does not contain. treeSHA is the applied state's tree when it has one; a
// legacy stash-identified state passes "" and everything untracked is
// preserved (its restore-time exists check keeps applied content
// authoritative, since there is no tree to compare against).
//
// It is a no-op under CleanAll, which is what keeps the default path
// byte-identical: no extra git command, no stash, no restore.
func (g *GitSandbox) evacuateUntracked(ctx context.Context, w *gitRunner, treeSHA string) (untrackedStash, error) {
	var st untrackedStash
	if g.cleanPolicy != CleanPreserveUntracked {
		return st, nil
	}

	untracked, err := gitNULList(ctx, w, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return st, fmt.Errorf("preserve untracked: list untracked files: %w", err)
	}

	// The target tree's paths are captured content the apply restores from the
	// index; evacuating them would race the apply. Its directory prefixes are
	// shapes the apply re-materializes.
	inTarget := map[string]bool{}
	targetDirs := map[string]bool{}
	if treeSHA != "" {
		paths, err := gitNULList(ctx, w, "ls-tree", "-r", "--name-only", "-z", treeSHA)
		if err != nil {
			return st, fmt.Errorf("preserve untracked: list target tree %s: %w", treeSHA, err)
		}
		for _, p := range paths {
			inTarget[p] = true
			for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
				targetDirs[d] = true
			}
		}
	}

	for _, p := range untracked {
		if !inTarget[p] {
			st.files = append(st.files, stashedFile{rel: p})
		}
	}

	// Empty directories never appear in `ls-files --others`, so walk the
	// collapsed untracked directory roots (--directory) and record every
	// directory shape the target tree will not recreate. Directories holding
	// evacuated files are re-created by the file restore; this covers the
	// purely empty ones and empty descendants of populated directories.
	roots, err := gitNULList(ctx, w, "ls-files", "--others", "--exclude-standard", "--directory", "-z")
	if err != nil {
		return st, fmt.Errorf("preserve untracked: list untracked directories: %w", err)
	}
	for _, r := range roots {
		if !strings.HasSuffix(r, "/") {
			continue // a file, not a collapsed directory
		}
		rootFS := filepath.Join(g.workDir(), filepath.FromSlash(strings.TrimSuffix(r, "/")))
		// A walk error leaves that subtree's shapes unrecorded rather than
		// failing the whole apply: the files still evacuate, and a shape we
		// could not inspect is not one we can promise to recreate.
		_ = filepath.WalkDir(rootFS, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil || d == nil || !d.IsDir() {
				return nil
			}
			rel, rerr := filepath.Rel(g.workDir(), p)
			if rerr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if rel == "." || targetDirs[rel] {
				return nil
			}
			st.dirs = append(st.dirs, rel)
			return nil
		})
	}

	if len(st.files) == 0 {
		return st, nil
	}
	toStash := st.files
	st.files = nil

	gitDir, err := w.run(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return untrackedStash{dirs: st.dirs}, fmt.Errorf("preserve untracked: resolve git dir: %w", err)
	}
	stashDir, err := os.MkdirTemp(gitDir, "goatherd-untracked-*")
	if err != nil {
		return untrackedStash{dirs: st.dirs}, fmt.Errorf("preserve untracked: create stash dir: %w", err)
	}
	st.dir = stashDir

	for _, f := range toStash {
		src := filepath.Join(g.workDir(), filepath.FromSlash(f.rel))
		dst := filepath.Join(stashDir, filepath.FromSlash(f.rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return st, fmt.Errorf("preserve untracked: stash dir for %s: %w", f.rel, err)
		}
		if err := os.Rename(src, dst); err != nil {
			if os.IsNotExist(err) {
				continue // vanished between listing and stashing
			}
			return st, fmt.Errorf("preserve untracked: stash %s: %w", f.rel, err)
		}
		f.stashed = dst
		st.files = append(st.files, f)
	}
	return st, nil
}

// restoreEvacuated recreates preserved directory shapes and moves evacuated
// files back. It never writes through a path component that is not a plain
// directory (the applied state may have reinstated a symlink or a file where a
// preserved directory used to be — following it could relocate content outside
// the repository) and never overwrites what the apply produced. Every refusal
// is reported in the returned error, which names the kept original; the caller
// keeps the stash whenever an error is returned. A nil result means the stash
// was fully drained and may be removed.
func (g *GitSandbox) restoreEvacuated(st untrackedStash) error {
	if st.empty() {
		if st.dir != "" {
			_ = os.RemoveAll(st.dir)
		}
		return nil
	}

	workDir := g.workDir()
	var errs []error

	for _, d := range st.dirs {
		p := filepath.Join(workDir, filepath.FromSlash(d))
		if !parentsSafe(workDir, d) {
			errs = append(errs, fmt.Errorf("directory %s not recreated: a parent path component is not a plain directory", d))
			continue
		}
		if _, err := os.Lstat(p); err == nil {
			continue // the apply already produced something here
		}
		if err := os.MkdirAll(p, 0o755); err != nil {
			errs = append(errs, fmt.Errorf("recreate directory %s: %w", d, err))
		}
	}

	for _, f := range st.files {
		if f.stashed == "" {
			continue // never moved
		}
		dst := filepath.Join(workDir, filepath.FromSlash(f.rel))
		if !parentsSafe(workDir, f.rel) {
			errs = append(errs, fmt.Errorf("%s not restored: a parent path component is not a plain directory (refusing to write through it); original kept at %s", f.rel, f.stashed))
			continue
		}
		if _, err := os.Lstat(dst); err == nil {
			errs = append(errs, fmt.Errorf("%s not restored: the applied state put content at that path (file/directory conflict); original kept at %s", f.rel, f.stashed))
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			errs = append(errs, fmt.Errorf("%s not restored: create parent: %w; original kept at %s", f.rel, err, f.stashed))
			continue
		}
		if err := os.Rename(f.stashed, dst); err != nil {
			errs = append(errs, fmt.Errorf("%s not restored: %w; original kept at %s", f.rel, err, f.stashed))
		}
	}

	if len(errs) > 0 {
		// Keep the stash: it holds the only copy of everything that could not
		// be moved back. A silent success over lost content is the exact
		// failure class this policy exists to prevent.
		return fmt.Errorf("%s: preserved untracked content could not be fully restored — originals kept in %s: %w", g.cleanPolicy, st.dir, errors.Join(errs...))
	}
	if st.dir != "" {
		_ = os.RemoveAll(st.dir)
	}
	return nil
}

// parentsSafe reports whether every existing component of rel's parent path is
// a plain directory. A symlink (or file) among the parents means a restore
// would follow it out of the intended tree — possibly out of the repository
// entirely — so it is refused.
func parentsSafe(root, rel string) bool {
	dir := path.Dir(path.Clean("/" + rel))
	if dir == "/" || dir == "." {
		return true
	}
	cur := root
	for _, part := range strings.Split(strings.TrimPrefix(dir, "/"), "/") {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			continue // absent components are created as plain directories
		}
		if err != nil || !fi.IsDir() {
			return false
		}
	}
	return true
}

// gitNULList runs a git command and splits its NUL-delimited output into
// non-empty entries. Paths with spaces, quotes, or newlines survive because the
// separator is the only delimiter consulted.
func gitNULList(ctx context.Context, w *gitRunner, args ...string) ([]string, error) {
	out, err := w.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var entries []string
	for _, e := range strings.Split(out, "\x00") {
		if e != "" {
			entries = append(entries, e)
		}
	}
	return entries, nil
}
