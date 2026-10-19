//go:build !windows

package winacl

import (
	"fmt"

	"github.com/buchenberg/shepherd-runtime-go/sandbox/containment"
)

// available reports the backend as unavailable off Windows.
func available() (bool, string) {
	return false, "the Windows write-restriction backend is Windows-only"
}

// probe refuses off Windows: there is no paddock, and a probe that cannot
// establish one must say so rather than pass vacuously.
func probe(profile, workingRoot string, roots []string) error {
	return fmt.Errorf("%w: the Windows write-restriction backend is Windows-only",
		containment.ErrPaddockNotEstablished)
}

// launch refuses off Windows rather than running the body unconfined.
func launch(profile string, req containment.LaunchRequest) (containment.LaunchResult, error) {
	return containment.LaunchResult{}, fmt.Errorf(
		"%w: the Windows write-restriction backend is Windows-only",
		containment.ErrPaddockNotEstablished)
}
