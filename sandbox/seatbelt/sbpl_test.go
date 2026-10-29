package seatbelt

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderSBPLDenyClosed(t *testing.T) {
	root := t.TempDir()
	profile := renderSBPL(profileDoc{
		Roots: []string{root},
		Net:   netDoc{Mode: modeDenyAll},
	})
	for _, want := range []string{
		"(version 1)\n",
		"(allow default)\n",
		"(deny file-write*)\n",
		"(deny network-outbound)\n",
		fmt.Sprintf("(allow file-write* (subpath \"%s\"))\n", sbplString(root)),
		fmt.Sprintf("(deny file-write* (literal \"%s\"))\n", sbplString(filepath.Join(root, ".git"))),
		fmt.Sprintf("(deny file-write* (subpath \"%s\"))\n", sbplString(filepath.Join(root, ".git"))),
		"(allow file-write* (subpath \"/dev\"))\n",
	} {
		if !strings.Contains(profile, want) {
			t.Fatalf("profile does not contain %q:\n%s", want, profile)
		}
	}
	if strings.Contains(profile, "(allow network-outbound") {
		t.Fatalf("deny_all profile admits network-outbound:\n%s", profile)
	}
	// The .git deny is broader-scoped than the root grant only in the file tree,
	// and must come after it so it wins under the profile's rule ordering.
	rootGrant := fmt.Sprintf("(allow file-write* (subpath \"%s\"))\n", sbplString(root))
	gitDeny := fmt.Sprintf("(deny file-write* (subpath \"%s\"))\n", sbplString(filepath.Join(root, ".git")))
	if strings.Index(profile, rootGrant) > strings.Index(profile, gitDeny) {
		t.Fatalf("the root grant comes after the .git deny:\n%s", profile)
	}
}

func TestRenderSBPLAllowAll(t *testing.T) {
	profile := renderSBPL(profileDoc{Net: netDoc{Mode: modeAllowAll}})
	if strings.Contains(profile, "network") {
		t.Fatalf("allow_all profile carries a network rule:\n%s", profile)
	}
}

func TestRenderSBPLBrokerPinsLoopbackPort(t *testing.T) {
	profile := renderSBPL(profileDoc{Net: netDoc{Mode: modeBroker, Port: 8080}})
	for _, want := range []string{
		"(deny network-outbound)\n",
		"(allow network-outbound (remote ip \"localhost:8080\"))\n",
		"(allow network-outbound (remote ip \"127.0.0.1:8080\"))\n",
	} {
		if !strings.Contains(profile, want) {
			t.Fatalf("broker profile does not contain %q:\n%s", want, profile)
		}
	}
}

func TestSBPLStringEscapes(t *testing.T) {
	cases := []struct{ in, want string }{
		{`/plain/path`, `/plain/path`},
		{`a\b`, `a\\b`},
		{`a"b`, `a\"b`},
		{"a\\\"b", `a\\\"b`},
	}
	for _, tc := range cases {
		if got := sbplString(tc.in); got != tc.want {
			t.Fatalf("sbplString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRenderSBPLUsesEscapedRoots(t *testing.T) {
	root := `/ws "quoted" \path`
	profile := renderSBPL(profileDoc{Roots: []string{root}, Net: netDoc{Mode: modeDenyAll}})
	want := fmt.Sprintf("(allow file-write* (subpath \"%s\"))\n", sbplString(root))
	if !strings.Contains(profile, want) {
		t.Fatalf("profile does not contain the escaped root %q:\n%s", want, profile)
	}
	if strings.Contains(profile, fmt.Sprintf("subpath \"%s\"", root)) {
		t.Fatalf("profile contains the raw root:\n%s", profile)
	}
}

func TestRenderSBPLNoRootsStillDeniesWrites(t *testing.T) {
	profile := renderSBPL(profileDoc{Net: netDoc{Mode: modeDenyAll}})
	if !strings.Contains(profile, "(deny file-write*)\n") {
		t.Fatalf("a rootless profile does not deny writes:\n%s", profile)
	}
	// Only the /dev allowance survives a rootless profile.
	if got := strings.Count(profile, "(allow file-write*"); got != 1 {
		t.Fatalf("a rootless profile carries %d write grants, want 1 (/dev):\n%s", got, profile)
	}
	if !strings.Contains(profile, `(allow file-write* (subpath "/dev"))`) {
		t.Fatalf("a rootless profile does not carry the /dev grant:\n%s", profile)
	}
}
