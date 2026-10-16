package confinement

import (
	"fmt"

	"github.com/buchenberg/shepherd-runtime-go/sandbox/containment"
)

// Placement is where authority is enforced for a run.
type Placement string

const (
	// PlacementJail requires a jail-capable backend; the run refuses otherwise.
	PlacementJail Placement = "jail"
	// PlacementAdvisory runs unconfined when no jail is available.
	PlacementAdvisory Placement = "advisory"
	// PlacementAuto jails when a backend is available, else falls back to advisory.
	PlacementAuto Placement = "auto"
)

// ResolvePlacement decides whether a run is jailed, given whether a jail is
// available on this host. jail refuses without one (fail-closed); advisory never
// jails; auto jails when available.
func ResolvePlacement(p Placement, available bool) (jail bool, err error) {
	switch p {
	case PlacementJail:
		if !available {
			return false, fmt.Errorf("%w: placement=jail requires a jail-capable backend, none available", containment.ErrJailNotEstablished)
		}
		return true, nil
	case PlacementAdvisory:
		return false, nil
	case PlacementAuto:
		return available, nil
	default:
		return false, fmt.Errorf("confinement: unknown placement %q", p)
	}
}
