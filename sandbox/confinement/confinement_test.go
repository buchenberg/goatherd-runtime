package confinement

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/buchenberg/goatherd-runtime/sandbox/containment"
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
	if paddock, err := ResolvePlacement(PlacementAuto, false); err != nil || paddock {
		t.Fatalf("auto, unavailable = (%v, %v)", paddock, err)
	}
	if paddock, err := ResolvePlacement(PlacementAuto, true); err != nil || !paddock {
		t.Fatalf("auto, available = (%v, %v)", paddock, err)
	}
	if _, err := ResolvePlacement(PlacementPaddock, false); !errors.Is(err, containment.ErrPaddockNotEstablished) {
		t.Fatalf("paddock, unavailable must refuse, got %v", err)
	}
	if paddock, err := ResolvePlacement(PlacementAdvisory, true); err != nil || paddock {
		t.Fatalf("advisory must never paddock, got (%v, %v)", paddock, err)
	}
}

func TestDeclaration_MayAndGrants(t *testing.T) {
	if d := MayDeclaration("ReadOnly", "m1"); d.May != "ReadOnly" || d.Match != "m1" || len(d.Grants) != 0 {
		t.Fatalf("MayDeclaration = %+v", d)
	}
	g := BindingRootGrant{Root: "/x", Writable: true}
	if d := GrantsDeclaration("m2", g); d.May != "" || d.Match != "m2" || len(d.Grants) != 1 {
		t.Fatalf("GrantsDeclaration = %+v", d)
	}
}

func TestInstall_MayAndGrants(t *testing.T) {
	root := t.TempDir()

	plan, err := Install(MayDeclaration("ReadOnly", "m1"), root)
	if err != nil {
		t.Fatalf("Install(may): %v", err)
	}
	if _, ok := plan.Monitor(PaddockMonitor); !ok {
		t.Fatal("plan must assign the paddock monitor")
	}
	if _, ok := plan.Monitor(CarrierMonitor); !ok {
		t.Fatal("plan must assign the carrier monitor")
	}
	spec, err := plan.Confinement()
	if err != nil {
		t.Fatalf("Confinement(): %v", err)
	}
	if len(spec.WritableRoots) != 0 {
		t.Fatalf("ReadOnly paddock spec = %+v, want no writable roots", spec)
	}

	rw := filepath.Join(root, "backend")
	if err := os.MkdirAll(rw, 0o755); err != nil {
		t.Fatal(err)
	}
	plan2, err := Install(GrantsDeclaration("m2", BindingRootGrant{Binding: "backend", Root: rw, Writable: true}), root)
	if err != nil {
		t.Fatalf("Install(grants): %v", err)
	}
	spec2, err := plan2.Confinement()
	if err != nil {
		t.Fatalf("Confinement(): %v", err)
	}
	if len(spec2.WritableRoots) != 1 || spec2.WritableRoots[0] != containment.Canonical(rw) {
		t.Fatalf("grant paddock spec = %+v", spec2)
	}
}

func TestInstall_EmptyGrantsIsReadOnly(t *testing.T) {
	plan, err := Install(GrantsDeclaration("m"), t.TempDir())
	if err != nil {
		t.Fatalf("Install(empty grants): %v", err)
	}
	spec, err := plan.Confinement()
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.WritableRoots) != 0 || spec.Network.Mode != containment.NetDenyAll {
		t.Fatalf("empty grants must lower to ReadOnly, got %+v", spec)
	}
}

func TestInstall_CarrierOnly(t *testing.T) {
	plan := InstallCarrierCheck(CarrierCheckAuthority{EffectiveMatchDigest: "e1", Route: "carrier_diff"})
	if _, ok := plan.Monitor(PaddockMonitor); ok {
		t.Fatal("a carrier-only plan must not assign the paddock monitor")
	}
	if _, err := plan.Confinement(); !errors.Is(err, ErrPlanEvidence) {
		t.Fatalf("Confinement() on a carrier-only plan = %v, want ErrPlanEvidence", err)
	}
}

func TestInstall_RefusesUnsupported(t *testing.T) {
	root := t.TempDir()
	if _, err := Install(MayDeclaration("Standard", "m"), root); !errors.Is(err, ErrUnsupportedMayProfile) {
		t.Fatalf("want ErrUnsupportedMayProfile, got %v", err)
	}
	rw := filepath.Join(root, "rw")
	_ = os.MkdirAll(rw, 0o755)
	_, err := Install(Declaration{May: "ReadOnly", Grants: []BindingRootGrant{{Root: rw, Writable: true}}}, root)
	if err == nil {
		t.Fatal("a declaration setting both May and Grants must be refused")
	}
}

func TestPermissionPlan_DigestStable(t *testing.T) {
	p := PlanForSpec(containment.ReadOnlySpec(), "match-1")
	if p.Digest() != p.Digest() {
		t.Fatal("digest must be stable")
	}
	if p.Fallback != "enforce" {
		t.Fatalf("fallback = %q, want enforce", p.Fallback)
	}
	if PlanForSpec(containment.ReadOnlySpec(), "match-2").Digest() == p.Digest() {
		t.Fatal("digest must change with the match digest")
	}
}

func TestPermissionPlan_ConfinementIsThePaddockSpec(t *testing.T) {
	want := containment.PermissiveFor(t.TempDir())
	plan := PlanForSpec(want, "")
	got, err := plan.Confinement()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.WritableRoots) != len(want.WritableRoots) || got.Network.Mode != want.Network.Mode {
		t.Fatalf("Confinement() = %+v, want the paddock spec %+v", got, want)
	}
}

func TestMonitorAssignment_Present(t *testing.T) {
	plan := PlanForSpec(containment.ReadOnlySpec(), "")
	paddock, ok := plan.Monitor(PaddockMonitor)
	if !ok || paddock.Timing != TimingPreAction || paddock.Confinement == nil {
		t.Fatalf("paddock assignment = %+v", paddock)
	}
	carrier, ok := plan.Monitor(CarrierMonitor)
	if !ok || carrier.Timing != TimingCommit || carrier.Confinement != nil {
		t.Fatalf("carrier assignment = %+v", carrier)
	}
}

func TestValidatePlanEvidence(t *testing.T) {
	p := PlanForSpec(containment.ReadOnlySpec(), "match-1")
	if err := ValidatePlanEvidence(p.Digest(), p, "", "match-1", ""); err != nil {
		t.Fatalf("matching evidence should validate: %v", err)
	}
	if err := ValidatePlanEvidence("bogus", p, "", "", ""); !errors.Is(err, ErrPlanEvidence) {
		t.Fatalf("wrong digest: want ErrPlanEvidence, got %v", err)
	}
	if err := ValidatePlanEvidence(p.Digest(), p, "", "wrong-match", ""); !errors.Is(err, ErrPlanEvidence) {
		t.Fatalf("wrong match digest: want ErrPlanEvidence, got %v", err)
	}
}
