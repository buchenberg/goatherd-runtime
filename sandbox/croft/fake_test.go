//go:build linux

package croft

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"time"

	croftlib "github.com/buchenberg/goatherd-croft"
)

// fakeCroft is an in-memory croftlib.Sandbox. It lets every conversion in this
// package be tested with no containerd daemon, no root and no overlayfs —
// croft's own fakes live in its fake_test.go and are not importable, but the
// interface they satisfy is exported, so the adapter can be driven through it.
//
// It models the one behaviour the adapter's contract tests actually depend on:
// Capture mints a *sequence-derived* snapshot key, exactly as real croft does,
// so re-capturing unchanged content yields a different state identity. That is
// the digest-stability gap the package doc records, and TestSandbox_
// CaptureDigestIsNotContentStable asserts it rather than hiding it.
type fakeCroft struct {
	mu sync.Mutex

	image string

	// seq drives the snapshot keys Capture mints.
	seq int
	// parent is the key the active layer was prepared from; base is the last
	// Applied state's key, the merge base ApplyDelta diffs against.
	parent string
	base   string

	files    map[string][]byte
	states   map[string]map[string][]byte
	released map[string]bool

	// changes and diffText are what Changes and Diff report, so a test can
	// pin the two halves of the Diff bridge independently.
	changes  []croftlib.Change
	diffText string

	// execFn, when set, replaces the default echo-back exec.
	execFn func(ctx context.Context, req croftlib.ExecRequest) (croftlib.ExecResult, error)

	// error injection, one per verb.
	createErr, destroyErr, captureErr, applyErr, execErr         error
	readErr, writeErr, changesErr, diffErr, deltaErr, releaseErr error

	// observations
	calls            []string
	lastExecReq      croftlib.ExecRequest
	lastExecDeadline time.Time
	lastCreateHadDL  bool
	applied          croftlib.WorkspaceState
	releasedStates   []croftlib.WorkspaceState
	created, gone    bool
}

func newFakeCroft(image string) *fakeCroft {
	return &fakeCroft{
		image:    image,
		parent:   "croft-base",
		files:    map[string][]byte{},
		states:   map[string]map[string][]byte{},
		released: map[string]bool{},
	}
}

func (f *fakeCroft) record(verb string) { f.calls = append(f.calls, verb) }

func (f *fakeCroft) Create(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("create")
	_, f.lastCreateHadDL = ctx.Deadline()
	if f.createErr != nil {
		return f.createErr
	}
	f.created = true
	return nil
}

func (f *fakeCroft) Destroy(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("destroy")
	if f.destroyErr != nil {
		return f.destroyErr
	}
	f.gone = true
	return nil
}

func (f *fakeCroft) Capture(context.Context) (croftlib.WorkspaceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("capture")
	if f.captureErr != nil {
		return croftlib.WorkspaceState{}, f.captureErr
	}
	f.seq++
	ws := croftlib.WorkspaceState{
		SnapshotKey: fmt.Sprintf("croft-seq-%d", f.seq),
		ParentKey:   f.parent,
		BaseKey:     f.base,
		Image:       f.image,
	}
	// Snapshot the tree under the new key so a later Changes/Diff against this
	// state has something to compare to, and pin it as a state.
	tree := make(map[string][]byte, len(f.files))
	for k, v := range f.files {
		tree[k] = v
	}
	f.states[ws.SnapshotKey] = tree
	f.parent = ws.SnapshotKey
	return ws, nil
}

func (f *fakeCroft) Apply(_ context.Context, ws croftlib.WorkspaceState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("apply")
	if f.applyErr != nil {
		return f.applyErr
	}
	if f.released[ws.SnapshotKey] {
		return croftlib.ErrStateUnavailable
	}
	tree, ok := f.states[ws.SnapshotKey]
	if !ok {
		return croftlib.ErrStateUnavailable
	}
	f.files = make(map[string][]byte, len(tree))
	for k, v := range tree {
		f.files[k] = v
	}
	// Apply rebinds: the applied state becomes both the active layer's parent
	// and the merge base for the next capture, matching croft's lineage rules.
	f.parent = ws.SnapshotKey
	f.base = ws.SnapshotKey
	f.applied = ws
	return nil
}

func (f *fakeCroft) ReleaseState(_ context.Context, ws croftlib.WorkspaceState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("release")
	if f.releaseErr != nil {
		return f.releaseErr
	}
	f.released[ws.SnapshotKey] = true
	delete(f.states, ws.SnapshotKey)
	f.releasedStates = append(f.releasedStates, ws)
	return nil
}

func (f *fakeCroft) Exec(ctx context.Context, req croftlib.ExecRequest) (croftlib.ExecResult, error) {
	f.mu.Lock()
	f.record("exec")
	f.lastExecReq = req
	f.lastExecDeadline, _ = ctx.Deadline()
	fn := f.execFn
	err := f.execErr
	f.mu.Unlock()

	if err != nil {
		return croftlib.ExecResult{}, err
	}
	if fn != nil {
		return fn(ctx, req)
	}
	return croftlib.ExecResult{
		ExitCode: 0,
		Stdout:   "ok " + req.Command,
		Duration: time.Millisecond,
	}, nil
}

func (f *fakeCroft) ReadFile(_ context.Context, path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("read")
	if f.readErr != nil {
		return nil, f.readErr
	}
	b, ok := f.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return b, nil
}

func (f *fakeCroft) WriteFile(_ context.Context, path string, data []byte, _ fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("write")
	if f.writeErr != nil {
		return f.writeErr
	}
	f.files[path] = data
	return nil
}

func (f *fakeCroft) Changes(_ context.Context, ws croftlib.WorkspaceState) ([]croftlib.Change, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("changes")
	if f.changesErr != nil {
		return nil, f.changesErr
	}
	if f.changes != nil {
		return f.changes, nil
	}
	// Default: derive from the recorded tree so an unpinned Changes call still
	// reports something truthful.
	base := f.states[ws.SnapshotKey]
	out := diffTrees(base, f.files)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (f *fakeCroft) Diff(_ context.Context, ws croftlib.WorkspaceState) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("diff")
	if f.diffErr != nil {
		return "", f.diffErr
	}
	if f.diffText != "" {
		return f.diffText, nil
	}
	var b []string
	for _, c := range diffTrees(f.states[ws.SnapshotKey], f.files) {
		b = append(b, fmt.Sprintf("%s %s", c.Kind, c.Path))
	}
	sort.Strings(b)
	if len(b) == 0 {
		return "", nil
	}
	return strings.Join(b, "\n"), nil
}

func (f *fakeCroft) ApplyDelta(_ context.Context, ws croftlib.WorkspaceState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("applydelta")
	if f.deltaErr != nil {
		return f.deltaErr
	}
	if ws.BaseKey == "" {
		return fmt.Errorf("croft: state %s has no merge base", ws.SnapshotKey)
	}
	tree, ok := f.states[ws.SnapshotKey]
	if !ok || f.released[ws.SnapshotKey] {
		return croftlib.ErrStateUnavailable
	}
	for k, v := range tree {
		f.files[k] = v
	}
	return nil
}

// diffTrees classifies the difference between a captured tree and a live one
// the way containerd's differ would: add, modify, delete.
func diffTrees(base, live map[string][]byte) []croftlib.Change {
	var out []croftlib.Change
	for p, v := range live {
		bv, ok := base[p]
		switch {
		case !ok:
			out = append(out, croftlib.Change{Path: p, Kind: croftlib.ChangeAdd, Size: int64(len(v))})
		case string(bv) != string(v):
			out = append(out, croftlib.Change{Path: p, Kind: croftlib.ChangeModify, Size: int64(len(v))})
		}
	}
	for p := range base {
		if _, ok := live[p]; !ok {
			out = append(out, croftlib.Change{Path: p, Kind: croftlib.ChangeDelete})
		}
	}
	return out
}
