package containment

import (
	"fmt"
)

// Placement is where authority is enforced for a run.
type Placement string

const (
	// PlacementPaddock requires a paddock-capable backend; the run refuses otherwise.
	PlacementPaddock Placement = "paddock"
	// PlacementAdvisory runs unconfined when no paddock is available.
	PlacementAdvisory Placement = "advisory"
	// PlacementAuto paddocks when a backend is available, else falls back to advisory.
	PlacementAuto Placement = "auto"
)

// ResolvePlacement decides whether a run is confined, given whether a paddock is
// available on this host. paddock refuses without one (fail-closed); advisory never
// paddocks; auto paddocks when available.
func ResolvePlacement(p Placement, available bool) (paddock bool, err error) {
	switch p {
	case PlacementPaddock:
		if !available {
			return false, fmt.Errorf("%w: placement=paddock requires a paddock-capable backend, none available", ErrPaddockNotEstablished)
		}
		return true, nil
	case PlacementAdvisory:
		return false, nil
	case PlacementAuto:
		return available, nil
	default:
		return false, fmt.Errorf("containment: unknown placement %q", p)
	}
}
