// Package effect turns a workspace change set into typed, causally-linked trace
// records plus live bus events. It is the Go analogue of the Shepherd
// reference's OverlayEffectExtractor.
//
// # What it is, and what it deliberately is not
//
// The Python reference extracts *reversible* effects: FileCreate carries the
// file's content, FilePatch carries old and new content, FileDelete carries what
// was there, and each has a reverse() so an effect can be undone. That works
// because Python walks the overlay upper layer itself and reads every file.
//
// This package does not do that, and cannot from the input it is given. A
// containerd-backed change set is metadata — path, kind, mode, size — because
// the daemon has already classified the difference between two snapshots.
// Reading content back for every changed path would mean a round trip per file
// through the container boundary, and for a large change set that is a
// materially different operation from the one this package performs.
//
// So these effects are **observations**, not reversible actions: a record that
// a path was created, patched or deleted, with enough metadata to reason about
// it, and no capacity to undo it. Plan 05 called this "parity"; it is not, and
// the distinction matters to anyone expecting to build rollback on top.
// Rollback in the Go stack is the workspace-state layer's job — Capture and
// Apply on a sandbox — not the effect layer's.
//
// # Why the input type is this package's own
//
// FromChanges takes []Change, defined here, rather than the change type of any
// particular backend. That is not abstraction for its own sake: this package
// lives in the runtime core, whose go.mod is deliberately free of any container
// or OCI dependency. Taking a containerd-backed type in the signature would pull
// containerd v2 and the whole OCI tree into the core, destroying the reason the
// container backend is a nested module at all. Backends convert into Change;
// the sandbox/croft adapter's EffectChanges will do exactly that, once the core
// is released with this package and the nested module can pin it.
//
// It also keeps the vocabulary reusable. The copy and clonefile carriers planned
// for later phases produce changes too, and none of them should have to route
// through a containerd type to emit an effect.
package effect

import (
	"fmt"
	"io/fs"
)

// Kind classifies a workspace change. The values are strings so they read
// meaningfully in a trace payload and in an error message, rather than as
// integers whose meaning lives in a const block nobody scrolls back to.
type Kind string

const (
	// KindCreated is a path that did not exist at the comparison point.
	KindCreated Kind = "created"
	// KindPatched is a path that existed and changed — content, mode, or both.
	KindPatched Kind = "patched"
	// KindDeleted is a path that existed at the comparison point and does not
	// now. A container backend resolves its own whiteouts, so a delete arrives
	// classified rather than as an opaque marker file.
	KindDeleted Kind = "deleted"
)

// Schema refs for the three file effects.
//
// They use the kernel's shepherd2. prefix, matching every existing ref
// (shepherd2.execution.created.v1, shepherd2.runtime.published_fact.v1, ...).
// The kernel's ValidateSchemaRef checks only non-emptiness, so introducing these
// needs no kernel ABI change — but the prefix is a convention worth keeping,
// since a ref that does not match its siblings is a ref nobody can find by
// pattern.
const (
	SchemaFileCreated = "shepherd2.effect.file.created.v1"
	SchemaFilePatched = "shepherd2.effect.file.patched.v1"
	SchemaFileDeleted = "shepherd2.effect.file.deleted.v1"
)

// Kind labels, the human-readable counterpart to the schema refs. Colon-separated
// like the kernel's existing labels ("turn:started").
const (
	LabelFileCreated = "file:created"
	LabelFilePatched = "file:patched"
	LabelFileDeleted = "file:deleted"
)

// SchemaRef returns the record schema for a kind.
func (k Kind) SchemaRef() (string, error) {
	switch k {
	case KindCreated:
		return SchemaFileCreated, nil
	case KindPatched:
		return SchemaFilePatched, nil
	case KindDeleted:
		return SchemaFileDeleted, nil
	}
	// No "effect:" prefix here: callers that wrap this add their own context,
	// and a prefix at both layers reads as a leaked inner frame.
	return "", fmt.Errorf("unknown change kind %q (valid: %q, %q, %q)", k, KindCreated, KindPatched, KindDeleted)
}

// KindLabel returns the human-readable label for a kind.
func (k Kind) KindLabel() (string, error) {
	switch k {
	case KindCreated:
		return LabelFileCreated, nil
	case KindPatched:
		return LabelFilePatched, nil
	case KindDeleted:
		return LabelFileDeleted, nil
	}
	return "", fmt.Errorf("unknown change kind %q (valid: %q, %q, %q)", k, KindCreated, KindPatched, KindDeleted)
}

// Change is one classified difference between a workspace and a comparison
// point, in backend-neutral terms.
//
// It carries metadata only. See the package doc for why that is a deliberate
// limit rather than an omission.
type Change struct {
	// Path is the changed path, workspace-relative and slash-separated,
	// matching what a sandbox Diff reports.
	Path string
	// Kind classifies the change.
	Kind Kind
	// IsDir marks a directory entry.
	IsDir bool
	// Mode is the entry's mode at the comparison point, including any
	// setuid/setgid/sticky bits the backend reports.
	Mode fs.FileMode
	// Size is the entry's size in bytes. Meaningless for a delete, where the
	// backend typically reports zero.
	Size int64
}

// ChangeEffect is a Change resolved to the schema ref and label it will be
// recorded under, ready for Emit.
//
// Splitting this from Change means the mapping is decided once, here, and a
// caller cannot append an effect under the wrong schema by hand.
//
// There is deliberately no IsDir field. FromChanges omits directories, so such
// a field could only ever be false, and a payload key that is constant across
// every record advertises information the pipeline structurally cannot produce
// — a consumer would be entitled to read is_dir:false as positive evidence that
// the path is a file. If directory effects are ever added, the field comes back
// with a kind that can actually be true.
type ChangeEffect struct {
	// SchemaRef is the record schema, one of the SchemaFile* constants.
	SchemaRef string
	// KindLabel is the human-readable label, one of the LabelFile* constants.
	KindLabel string
	// Path, Mode and Size are carried through from the Change.
	Path string
	Mode fs.FileMode
	Size int64
}

// FromChanges maps a change set onto typed effects.
//
// Directories are skipped, matching the Python reference, which notes that a
// directory creation is "usually implicit" and defers DirectoryCreate effects.
// A directory carries no content to observe, and its existence is already
// implied by any child effect beneath it. The sandbox Diff path list drops
// directories for the same reason, so the two views of a change set agree.
//
// An unrecognised Kind is an error rather than a skipped entry. Silently
// dropping a change a backend reported would mean a workspace mutation goes
// unrecorded and nothing says so — the failure mode this package exists to
// prevent. It also means a backend gaining a new kind (a rename, say) surfaces
// as a compile-or-test failure here instead of a quiet hole in the trace, and
// adding the kind becomes a deliberate act that includes choosing its schema.
func FromChanges(changes []Change) ([]ChangeEffect, error) {
	out := make([]ChangeEffect, 0, len(changes))
	for _, c := range changes {
		if c.IsDir {
			continue
		}
		ref, err := c.Kind.SchemaRef()
		if err != nil {
			return nil, fmt.Errorf("effect: path %s: %w", c.Path, err)
		}
		label, err := c.Kind.KindLabel()
		if err != nil {
			return nil, fmt.Errorf("effect: path %s: %w", c.Path, err)
		}
		out = append(out, ChangeEffect{
			SchemaRef: ref,
			KindLabel: label,
			Path:      c.Path,
			Mode:      c.Mode,
			Size:      c.Size,
		})
	}
	return out, nil
}

// UnixOctal renders a mode the way `stat -c %a` would, including the special
// bits — "4755" for a setuid executable, not "0755".
//
// It is the inverse of the parsing a sandbox backend does when it shells out to
// stat, and it is deliberately lossless where a host-side write is not: an
// effect is an observation, so recording the setuid bit is information, whereas
// propagating it onto a host file would be a privilege decision. Keeping the
// full mode here means the trace can answer "what was this file's mode" even
// for bits that never cross back to the host.
func UnixOctal(m fs.FileMode) string {
	var o uint32 = uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		o |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		o |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		o |= 0o1000
	}
	return fmt.Sprintf("%04o", o)
}

// Payload renders an effect as a trace record payload.
//
// causedBy is the operation the effect is attributed to — the tool call or run
// that produced it — and is omitted when empty rather than recorded as an empty
// string, so a consumer can distinguish "no known cause" from "the cause was
// the empty string".
//
// There is no is_dir key: FromChanges omits directories, so every record this
// produces is a file, and a key that is constant across all of them is noise at
// best and misleading at worst.
func (e ChangeEffect) Payload(causedBy string) map[string]any {
	p := map[string]any{
		"path": e.Path,
		"mode": UnixOctal(e.Mode),
		"size": e.Size,
	}
	if causedBy != "" {
		p["caused_by"] = causedBy
	}
	return p
}
