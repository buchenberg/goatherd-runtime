package confinement

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/buchenberg/shepherd-runtime-go/sandbox/containment"
)

func TestResolveMay_DefaultPermissive(t *testing.T) {
	r := ResolveMay("")
	if r.Resolved != "Permissive" || r.Source != "defaulted" || r.Declared != "" {
		t.Fatalf("ResolveMay(\"\") = %+v", r)
	}
	r = ResolveMay("ReadOnly")
	if r.Resolved != "ReadOnly" || r.Source != "declared" {
		t.Fatalf("ResolveMay(ReadOnly) = %+v", r)
	}
}

func TestLowerMay_ReadOnlyPermissive(t *testing.T) {
	spec, err := LowerMay("ReadOnly", "/x")
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.WritableRoots) != 0 {
		t.Fatalf("ReadOnly must have no writable root, got %v", spec.WritableRoots)
	}
	spec, err = LowerMay("Permissive", "/x")
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.WritableRoots) != 1 || spec.WritableRoots[0] != "/x" {
		t.Fatalf("Permissive roots = %v", spec.WritableRoots)
	}
}

func TestLowerMay_RefusesUnknown(t *testing.T) {
	if _, err := LowerMay("Standard", "/x"); !errors.Is(err, ErrUnsupportedMayProfile) {
		t.Fatalf("want ErrUnsupportedMayProfile, got %v", err)
	}
}

func TestLowerGrants_ReadOnlyContributesNothing(t *testing.T) {
	root := t.TempDir()
	rw := filepath.Join(root, "backend")
	ro := filepath.Join(root, "docs")
	for _, d := range []string{rw, ro} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	spec, err := LowerGrants([]BindingRootGrant{
		{Binding: "backend", Root: rw, Writable: true},
		{Binding: "docs", Root: ro, Writable: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.WritableRoots) != 1 || spec.WritableRoots[0] != containment.Canonical(rw) {
		t.Fatalf("writable roots = %v, want [%s]", spec.WritableRoots, containment.Canonical(rw))
	}
	if spec.Network.Mode != containment.NetDenyAll {
		t.Fatalf("grant lowering must default to deny-all, got %q", spec.Network.Mode)
	}
}

func TestValidateDisjointRoots_NestingRejected(t *testing.T) {
	root := t.TempDir()
	inner := filepath.Join(root, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateDisjointRoots([]string{root, inner}); !errors.Is(err, ErrOverlappingBoundRoots) {
		t.Fatalf("want ErrOverlappingBoundRoots, got %v", err)
	}
}

func TestValidateDisjointRoots_DistinctOK(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	got, err := ValidateDisjointRoots([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestValidateDisjointRoots_AliasRejected(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(filepath.Dir(root), "alias-link")
	_ = os.Remove(link)
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	defer os.Remove(link)
	if _, err := ValidateDisjointRoots([]string{root, link}); !errors.Is(err, ErrOverlappingBoundRoots) {
		t.Fatalf("want ErrOverlappingBoundRoots for an alias, got %v", err)
	}
}

func TestValidateDisjointRoots_UnicodeAliasRejected(t *testing.T) {
	base := t.TempDir()
	composed := filepath.Join(base, "\u00e9")    // é as one code point
	decomposed := filepath.Join(base, "e\u0301") // e + combining acute
	for _, d := range []string{composed, decomposed} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// On a normalization-sensitive filesystem these are distinct directories
	// (different inodes), so only the NFC fold catches the alias.
	if _, err := ValidateDisjointRoots([]string{composed, decomposed}); !errors.Is(err, ErrOverlappingBoundRoots) {
		t.Fatalf("want ErrOverlappingBoundRoots for a Unicode alias, got %v", err)
	}
}

func TestPlacement_AutoResolves(t *testing.T) {
	if jail, err := ResolvePlacement(PlacementAuto, false); err != nil || jail {
		t.Fatalf("auto, unavailable = (%v, %v)", jail, err)
	}
	if jail, err := ResolvePlacement(PlacementAuto, true); err != nil || !jail {
		t.Fatalf("auto, available = (%v, %v)", jail, err)
	}
	if _, err := ResolvePlacement(PlacementJail, false); !errors.Is(err, containment.ErrJailNotEstablished) {
		t.Fatalf("jail, unavailable must refuse, got %v", err)
	}
	if jail, err := ResolvePlacement(PlacementAdvisory, true); err != nil || jail {
		t.Fatalf("advisory must never jail, got (%v, %v)", jail, err)
	}
}

func TestPermissionPlan_DigestStable(t *testing.T) {
	p := JailPlan("match-1", "spec-1")
	if p.Digest() != p.Digest() {
		t.Fatal("digest must be stable")
	}
	if p.Assignments[0].Monitor != JailMonitor || p.Assignments[0].Fallback != "refuse" {
		t.Fatalf("assignment = %+v", p.Assignments[0])
	}
	if JailPlan("match-2", "spec-1").Digest() == p.Digest() {
		t.Fatal("digest must change with input")
	}
}
