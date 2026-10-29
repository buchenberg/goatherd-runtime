package seatbelt

import (
	"path/filepath"
	"testing"

	"github.com/buchenberg/goatherd-runtime/sandbox/confinement"
)

func mustParse(t *testing.T, profile string) profileDoc {
	t.Helper()
	doc, err := parseProfile(profile)
	if err != nil {
		t.Fatalf("parse profile: %v", err)
	}
	return doc
}

func TestProfileForReadOnly(t *testing.T) {
	b := New()
	doc := mustParse(t, b.ProfileFor(nil, confinement.NetworkPolicy{Mode: confinement.NetDenyAll}))
	if len(doc.Roots) != 0 {
		t.Fatalf("read-only profile carries %d roots", len(doc.Roots))
	}
	if doc.Net.Mode != modeDenyAll {
		t.Fatalf("network mode is %q, want %q", doc.Net.Mode, modeDenyAll)
	}
	if err := doc.validate(); err != nil {
		t.Fatalf("read-only profile does not validate: %v", err)
	}
}

func TestProfileForCanonicalizesRoots(t *testing.T) {
	root := t.TempDir()
	doc := mustParse(t, New().ProfileFor([]string{root, ""}, confinement.NetworkPolicy{Mode: confinement.NetAllowAll}))
	want := confinement.Canonical(root)
	if len(doc.Roots) != 1 || doc.Roots[0] != want {
		t.Fatalf("roots = %v, want [%s]", doc.Roots, want)
	}
	if doc.Net.Mode != modeAllowAll {
		t.Fatalf("network mode is %q, want %q", doc.Net.Mode, modeAllowAll)
	}
	if doc.Net.Port != 0 {
		t.Fatalf("allow_all carries port %d", doc.Net.Port)
	}
}

func TestProfileForDefaultsToDenyAll(t *testing.T) {
	for _, mode := range []confinement.NetMode{"", "not-a-mode"} {
		doc := mustParse(t, New().ProfileFor(nil, confinement.NetworkPolicy{Mode: mode}))
		if doc.Net.Mode != modeDenyAll {
			t.Fatalf("mode %q lowered to %q, want %q", mode, doc.Net.Mode, modeDenyAll)
		}
	}
}

func TestProfileForBroker(t *testing.T) {
	doc := mustParse(t, New().ProfileFor(nil, confinement.NetworkPolicy{Mode: confinement.NetBroker, BrokerPort: 8080}))
	if doc.Net.Mode != modeBroker || doc.Net.Port != 8080 {
		t.Fatalf("broker profile = %+v, want broker:8080", doc.Net)
	}
	if err := doc.validate(); err != nil {
		t.Fatalf("broker profile does not validate: %v", err)
	}
}

func TestProfileForBrokerWithoutPortFailsClosed(t *testing.T) {
	for _, port := range []int{0, -1, 65536} {
		doc := mustParse(t, New().ProfileFor(nil, confinement.NetworkPolicy{Mode: confinement.NetBroker, BrokerPort: port}))
		if doc.Net.Mode != modeDenyAll || doc.Net.Port != 0 {
			t.Fatalf("broker port %d lowered to %+v, want deny_all", port, doc.Net)
		}
	}
}

func TestProfileValidate(t *testing.T) {
	abs := t.TempDir()
	cases := []struct {
		name string
		doc  profileDoc
		ok   bool
	}{
		{"allow all", profileDoc{Net: netDoc{Mode: modeAllowAll}}, true},
		{"deny all", profileDoc{Net: netDoc{Mode: modeDenyAll}}, true},
		{"broker", profileDoc{Net: netDoc{Mode: modeBroker, Port: 8080}}, true},
		{"broker without port", profileDoc{Net: netDoc{Mode: modeBroker}}, false},
		{"broker with bad port", profileDoc{Net: netDoc{Mode: modeBroker, Port: 70000}}, false},
		{"allow all with port", profileDoc{Net: netDoc{Mode: modeAllowAll, Port: 1}}, false},
		{"unknown mode", profileDoc{Net: netDoc{Mode: "permissive"}}, false},
		{"empty root", profileDoc{Roots: []string{""}, Net: netDoc{Mode: modeDenyAll}}, false},
		{"relative root", profileDoc{Roots: []string{"relative"}, Net: netDoc{Mode: modeDenyAll}}, false},
		{"absolute root", profileDoc{Roots: []string{abs}, Net: netDoc{Mode: modeDenyAll}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.doc.validate()
			if tc.ok && err != nil {
				t.Fatalf("validate: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("validate accepted an invalid profile")
			}
		})
	}
}

func TestCanonicalAllDropsEmpty(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	got := canonicalAll([]string{"", first, "", second})
	want := []string{confinement.Canonical(first), confinement.Canonical(second)}
	if len(got) != len(want) {
		t.Fatalf("canonicalAll returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("canonicalAll[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWithinAnyRoot(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b")
	sibling := filepath.Join(filepath.Dir(root), "sibling")
	if !withinAnyRoot(root, []string{root}) {
		t.Fatal("the root itself is not within its own root")
	}
	if !withinAnyRoot(nested, []string{root}) {
		t.Fatal("a nested path is not within its root")
	}
	if withinAnyRoot(sibling, []string{root}) {
		t.Fatal("a sibling path was treated as within the root")
	}
	if withinAnyRoot(root, nil) {
		t.Fatal("a path was within an empty root set")
	}
}
