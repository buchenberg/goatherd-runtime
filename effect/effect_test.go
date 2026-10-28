package effect

import (
	"io/fs"
	"strings"
	"testing"
)

func TestFromChanges_MapsEachKindToItsSchema(t *testing.T) {
	changes := []Change{
		{Path: "new.txt", Kind: KindCreated, Mode: 0o644, Size: 5},
		{Path: "old.txt", Kind: KindPatched, Mode: 0o600, Size: 9},
		{Path: "gone.txt", Kind: KindDeleted},
	}

	got, err := FromChanges(changes)
	if err != nil {
		t.Fatalf("FromChanges: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d effects, want 3", len(got))
	}

	want := []struct{ ref, label string }{
		{SchemaFileCreated, LabelFileCreated},
		{SchemaFilePatched, LabelFilePatched},
		{SchemaFileDeleted, LabelFileDeleted},
	}
	for i, w := range want {
		if got[i].SchemaRef != w.ref || got[i].KindLabel != w.label {
			t.Errorf("effect %d = (%q, %q), want (%q, %q)", i, got[i].SchemaRef, got[i].KindLabel, w.ref, w.label)
		}
	}
	// Order must survive: a consumer correlating effects against a diff relies
	// on the change set's ordering.
	if got[0].Path != "new.txt" || got[1].Path != "old.txt" || got[2].Path != "gone.txt" {
		t.Errorf("order not preserved: %q %q %q", got[0].Path, got[1].Path, got[2].Path)
	}
	// Metadata carries through unchanged.
	if got[0].Size != 5 || got[0].Mode != 0o644 {
		t.Errorf("metadata lost: size=%d mode=%v", got[0].Size, got[0].Mode)
	}
}

func TestFromChanges_SkipsDirectories(t *testing.T) {
	// Matches the Python reference, which notes a directory creation is
	// "usually implicit". A directory has no content to observe, and its
	// existence is implied by any child effect beneath it.
	got, err := FromChanges([]Change{
		{Path: "pkg", Kind: KindCreated, IsDir: true, Mode: 0o755},
		{Path: "pkg/a.go", Kind: KindCreated, Mode: 0o644, Size: 10},
	})
	if err != nil {
		t.Fatalf("FromChanges: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d effects, want 1 (the directory must be skipped)", len(got))
	}
	if got[0].Path != "pkg/a.go" {
		t.Errorf("kept %q, want the file not the directory", got[0].Path)
	}
}

func TestFromChanges_OnlyDirectoriesYieldsEmpty(t *testing.T) {
	got, err := FromChanges([]Change{{Path: "a", Kind: KindCreated, IsDir: true}})
	if err != nil {
		t.Fatalf("FromChanges: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d effects, want 0", len(got))
	}
}

func TestFromChanges_EmptyInput(t *testing.T) {
	got, err := FromChanges(nil)
	if err != nil {
		t.Fatalf("FromChanges(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d effects, want 0", len(got))
	}
}

func TestFromChanges_UnknownKindIsAnError(t *testing.T) {
	// Silently dropping a change a backend reported would leave a workspace
	// mutation unrecorded with nothing saying so. It also means a backend
	// gaining a kind surfaces here instead of as a quiet hole in the trace.
	_, err := FromChanges([]Change{
		{Path: "ok.txt", Kind: KindCreated},
		{Path: "moved.txt", Kind: Kind("renamed")},
	})
	if err == nil {
		t.Fatal("an unknown change kind must be an error, not a skipped entry")
	}
	if !strings.Contains(err.Error(), "moved.txt") {
		t.Errorf("the error must name the offending path, got: %v", err)
	}
	if !strings.Contains(err.Error(), `"renamed"`) {
		t.Errorf("the error must name the unknown kind, got: %v", err)
	}
	for _, valid := range []string{"created", "patched", "deleted"} {
		if !strings.Contains(err.Error(), valid) {
			t.Errorf("the error must list %q as valid, got: %v", valid, err)
		}
	}
}

func TestKind_SchemaAndLabelAgree(t *testing.T) {
	// Every kind must resolve on both axes; a kind with a schema but no label
	// would append fine and publish an event a supervisor cannot name.
	for _, k := range []Kind{KindCreated, KindPatched, KindDeleted} {
		ref, rerr := k.SchemaRef()
		label, lerr := k.KindLabel()
		if rerr != nil || lerr != nil {
			t.Errorf("kind %q: schema err %v, label err %v", k, rerr, lerr)
			continue
		}
		if ref == "" || label == "" {
			t.Errorf("kind %q resolved to an empty ref or label", k)
		}
		// The schema prefix is the kernel's convention; a ref that does not
		// match its siblings is one nobody can find by pattern.
		if !strings.HasPrefix(ref, "shepherd2.") || !strings.HasSuffix(ref, ".v1") {
			t.Errorf("schema %q does not follow the shepherd2.*.v1 convention", ref)
		}
		if !strings.Contains(label, ":") {
			t.Errorf("label %q is not colon-separated like the kernel's existing labels", label)
		}
	}
}

func TestUnixOctal(t *testing.T) {
	cases := []struct {
		name string
		mode fs.FileMode
		want string
	}{
		{"plain file", 0o644, "0644"},
		{"executable", 0o755, "0755"},
		{"private", 0o600, "0600"},
		{"zero", 0, "0000"},
		{"setuid", fs.ModeSetuid | 0o755, "4755"},
		{"setgid", fs.ModeSetgid | 0o755, "2755"},
		{"sticky", fs.ModeSticky | 0o777, "1777"},
		{"all special bits", fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky | 0o750, "7750"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UnixOctal(tc.mode); got != tc.want {
				t.Errorf("UnixOctal(%v) = %q, want %q", tc.mode, got, tc.want)
			}
		})
	}
}

func TestUnixOctal_IgnoresTypeBits(t *testing.T) {
	// A mode carrying a type bit (a directory, a symlink) must render as the
	// permission bits only — the type is a separate field, and folding it into
	// the octal would make the payload unreadable as a mode.
	if got := UnixOctal(fs.ModeDir | 0o755); got != "0755" {
		t.Errorf("UnixOctal(dir 0755) = %q, want %q", got, "0755")
	}
	if got := UnixOctal(fs.ModeSymlink | 0o777); got != "0777" {
		t.Errorf("UnixOctal(symlink 0777) = %q, want %q", got, "0777")
	}
}

func TestPayload(t *testing.T) {
	e := ChangeEffect{
		SchemaRef: SchemaFilePatched,
		KindLabel: LabelFilePatched,
		Path:      "src/main.go",
		Mode:      fs.ModeSetuid | 0o755,
		Size:      2048,
	}

	withCause := e.Payload("run:42")
	for k, want := range map[string]any{
		"path":      "src/main.go",
		"mode":      "4755",
		"size":      int64(2048),
		"caused_by": "run:42",
	} {
		if got := withCause[k]; got != want {
			t.Errorf("payload[%q] = %#v, want %#v", k, got, want)
		}
	}
	// FromChanges omits directories, so every record is a file and an is_dir key
	// could only ever be false. A constant key is noise at best and reads as
	// evidence at worst; if directories ever get their own effect kind, this
	// assertion is the reminder to put the key back with a value that varies.
	if _, ok := withCause["is_dir"]; ok {
		t.Errorf("payload carries is_dir = %#v, but no effect this package produces can be a directory", withCause["is_dir"])
	}

	// An unknown cause is omitted, not recorded as an empty string, so a
	// consumer can tell "no known cause" from a cause that happened to be "".
	without := e.Payload("")
	if _, ok := without["caused_by"]; ok {
		t.Errorf("caused_by must be absent when there is no cause, got %#v", without["caused_by"])
	}
	if len(without) != 3 {
		t.Errorf("payload without a cause has %d keys, want 3 (path, mode, size): %#v", len(without), without)
	}
}

func TestPayload_ModeIsLossless(t *testing.T) {
	// An effect is an observation, so it records the setuid bit even though a
	// host-side write would not propagate it. The trace must be able to answer
	// "what was this file's mode" for bits that never cross back to the host.
	e := ChangeEffect{Path: "bin/tool", Mode: fs.ModeSetuid | 0o755}
	if got := e.Payload("run:1")["mode"]; got != "4755" {
		t.Errorf("mode = %v, want 4755 — the setuid bit was dropped from the observation", got)
	}
}
