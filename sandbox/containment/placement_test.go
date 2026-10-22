package containment

import (
	"errors"
	"testing"
)

func TestPlacement_AutoResolves(t *testing.T) {
	if paddock, err := ResolvePlacement(PlacementAuto, false); err != nil || paddock {
		t.Fatalf("auto, unavailable = (%v, %v)", paddock, err)
	}
	if paddock, err := ResolvePlacement(PlacementAuto, true); err != nil || !paddock {
		t.Fatalf("auto, available = (%v, %v)", paddock, err)
	}
	if _, err := ResolvePlacement(PlacementPaddock, false); !errors.Is(err, ErrPaddockNotEstablished) {
		t.Fatalf("paddock, unavailable must refuse, got %v", err)
	}
	if paddock, err := ResolvePlacement(PlacementAdvisory, true); err != nil || paddock {
		t.Fatalf("advisory must never paddock, got (%v, %v)", paddock, err)
	}
}
