//go:build linux

package landlock

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/buchenberg/shepherd-runtime-go/sandbox/containment"
)

// Landlock raw syscall numbers, sourced from x/sys so non-amd64 ports are
// covered. glibc lacks wrappers pre-2.36, hence the direct syscalls.
const (
	sysCreateRuleset = unix.SYS_LANDLOCK_CREATE_RULESET
	sysAddRule       = unix.SYS_LANDLOCK_ADD_RULE
	sysRestrictSelf  = unix.SYS_LANDLOCK_RESTRICT_SELF

	createRulesetVersion = 1 // LANDLOCK_CREATE_RULESET_VERSION
	rulePathBeneath      = 1 // LANDLOCK_RULE_PATH_BENEATH
)

// WriteAccessMask is the v1-compatible filesystem write-access mask, bit i ==
// (1 << i):
//
//	1    WRITE_FILE
//	4    REMOVE_DIR
//	5    REMOVE_FILE
//	6..12 MAKE_{CHAR,DIR,REG,SOCK,FIFO,BLOCK,SYM}
//
// Deliberately EXCLUDED so a v1 (kernel 5.13) ruleset is accepted:
//
//	13 REFER (v2), 14 TRUNCATE (v3), 15 IOCTL_DEV (v5).
//
// Reads/exec (bits 0,2,3) are NOT governed, so the sandbox may read the loader,
// /usr/lib, /bin, and dependencies without enumerating the filesystem.
//
// Ported verbatim from Python's _WRITE_ACCESS (_landlock_containment.py:35-40).
// Two facts are inherited knowingly: excluding REFER/TRUNCATE is a v1
// portability choice, and it means truncation of an otherwise-reachable path is
// not governed. Parity means replicating both, not silently diverging.
const WriteAccessMask = (1 << 1) | (1 << 4) | (1 << 5) | (1 << 6) | (1 << 7) |
	(1 << 8) | (1 << 9) | (1 << 10) | (1 << 11) | (1 << 12)

// rulesetAttr mirrors struct landlock_ruleset_attr at ABI v1: a single u64.
// Passing only 8 bytes keeps landlock_create_ruleset accepted by v1 kernels (a
// larger struct returns E2BIG on them).
type rulesetAttr struct {
	HandledAccessFS uint64
}

// pathBeneathAttr mirrors struct landlock_path_beneath_attr, which the kernel
// declares __attribute__((packed)): u64 + s32 = 12 bytes. The trailing pad makes
// the Go struct 16-byte aligned; the kernel copies only the first 12 bytes, and
// Go does not reorder fields, so the layout matches the kernel's view.
type pathBeneathAttr struct {
	AllowedAccess uint64
	ParentFD      int32
	_             uint32
}

// abi returns the kernel Landlock ABI version (>= 1 when supported), or an error
// wrapping ErrUnsupportedKernel. It distinguishes a missing syscall (ENOSYS) from
// one blocked by a sandbox (EPERM), the latter common under default container
// seccomp profiles.
func abi() (int, error) {
	r1, _, errno := unix.Syscall(sysCreateRuleset, 0, 0, createRulesetVersion)
	if errno != 0 {
		if errno == unix.EPERM || errno == unix.EACCES {
			return 0, fmt.Errorf("%w: landlock_create_ruleset: %v (blocked — e.g. a container seccomp profile denies syscalls 444-446)", containment.ErrUnsupportedKernel, errno)
		}
		return 0, fmt.Errorf("%w: landlock_create_ruleset: %v", containment.ErrUnsupportedKernel, errno)
	}
	version := int(r1)
	if version < 1 {
		return 0, fmt.Errorf("%w: reported ABI %d", containment.ErrUnsupportedKernel, version)
	}
	return version, nil
}

// confine restricts the calling thread — and, through it, its children — so that
// filesystem writes are permitted only beneath roots. Reads and exec are not
// governed. Any setup failure returns an error so the caller can fail closed.
func confine(roots []string) error {
	attr := rulesetAttr{HandledAccessFS: WriteAccessMask}
	fd, _, errno := unix.Syscall(sysCreateRuleset,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("landlock_create_ruleset: %w", errno)
	}
	defer func() { _ = unix.Close(int(fd)) }()

	for _, root := range roots {
		dirfd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open writable root %q: %w", root, err)
		}
		rule := pathBeneathAttr{AllowedAccess: WriteAccessMask, ParentFD: int32(dirfd)}
		_, _, addErr := unix.Syscall6(sysAddRule, fd, rulePathBeneath,
			uintptr(unsafe.Pointer(&rule)), 0, 0, 0)
		_ = unix.Close(dirfd)
		if addErr != 0 {
			return fmt.Errorf("landlock_add_rule %q: %w", root, addErr)
		}
	}

	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", err)
	}
	if _, _, errno := unix.Syscall(sysRestrictSelf, fd, 0, 0); errno != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", errno)
	}
	return nil
}
