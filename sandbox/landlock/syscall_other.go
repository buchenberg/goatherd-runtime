//go:build !linux

package landlock

import (
	"fmt"

	"github.com/buchenberg/shepherd-runtime-go/sandbox/containment"
)

// WriteAccessMask is defined for parity of the package's exported surface; the
// Linux build carries the real value. It is never used on non-Linux hosts.
const WriteAccessMask = 0

func abi() (int, error) {
	return 0, fmt.Errorf("%w: Landlock is Linux-only", containment.ErrUnsupportedKernel)
}

func confine([]string) error {
	return fmt.Errorf("%w: Landlock is Linux-only", containment.ErrUnsupportedKernel)
}
