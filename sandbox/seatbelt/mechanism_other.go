//go:build !darwin

package seatbelt

import (
	"fmt"

	"github.com/buchenberg/goatherd-runtime/sandbox/containment"
)

// available reports the backend as unavailable off macOS.
func available() (bool, string) {
	return false, "the Seatbelt backend is macOS-only"
}

// probe refuses off macOS: there is no sandbox, and a probe that cannot
// establish one must say so rather than pass vacuously.
func probe(profile, workingRoot string, roots []string) error {
	return fmt.Errorf("%w: the Seatbelt backend is macOS-only",
		containment.ErrPaddockNotEstablished)
}

// launch refuses off macOS rather than running the body unconfined.
func launch(profile string, req containment.LaunchRequest) (containment.LaunchResult, error) {
	return containment.LaunchResult{}, fmt.Errorf(
		"%w: the Seatbelt backend is macOS-only",
		containment.ErrPaddockNotEstablished)
}
