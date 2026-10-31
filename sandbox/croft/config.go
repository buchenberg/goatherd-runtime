package croft

import (
	"context"
	"errors"

	trace "github.com/buchenberg/goatherd-kernel"
	"github.com/buchenberg/goatherd-runtime/effect"
)

// BackendName is the registry key this backend answers to, and the value
// stamped into every trace.WorkspaceState it produces. The kernel's
// WorkspaceState doc names "containerd" as the container backend's identifier,
// and Apply rejects a state carrying a different one.
//
// It is declared here rather than in the Linux implementation so a host can
// reference the name — in config validation, in a doctor check — on any
// platform, including one where no such backend can ever be opened.
const BackendName = "containerd"

// EffectChanger is the optional capability this backend provides for typed
// workspace effects: the change set between a captured state and the current
// workspace, already translated into the runtime's backend-neutral vocabulary.
//
// It is declared here, untagged, for the same reason BackendName and Config are:
// a host must be able to name the capability on every platform it builds for.
// Callers detect it by type assertion, the convention the runtime itself uses
// for DeltaApplier, so a host never has to import croft to consume the result —
// which is the whole point of the translation. On a non-Linux build nothing
// implements it, because no backend registers; the assertion fails, and that is
// the honest answer rather than a stub returning empty changes.
//
// The result feeds effect.FromChanges and then effect.Emit. See EffectChanges
// for what the translation preserves and the one detail it cannot carry into a
// recorded payload.
type EffectChanger interface {
	EffectChanges(ctx context.Context, ws trace.WorkspaceState) ([]effect.Change, error)
}

// ErrUnsupportedPlatform is what New returns on a platform the backend is not
// built for. See the package doc: the implementation is Linux-only because
// goatherd-croft does not compile elsewhere and the substrate it drives
// (overlayfs, bind mounts, FIFOs) is Linux-shaped.
//
// Hosts should treat this as a configuration error, not a runtime surprise: a
// client that can reach it has already been pointed at the containerd backend
// on a platform that cannot provide one.
var ErrUnsupportedPlatform = errors.New("croft sandbox: the containerd backend requires Linux (containerd + overlayfs); this platform builds the stub only")

// Config configures the containerd backend. It is adapter-owned and mirrors
// the croft essentials field-for-field so a host never imports croft types —
// the same reason GitSandboxConfig exists for the git backend.
//
// It is declared untagged so the same configuration struct compiles on every
// platform. That is deliberate: it is what lets a host carry containerd
// settings in one config type, and validate or reject them, without a build
// tag of its own. On a non-Linux build the fields are inert and New refuses.
type Config struct {
	// Address is the containerd socket or endpoint. Empty uses containerd's
	// own default resolution.
	Address string
	// Namespace isolates this backend's containers and snapshots from other
	// containerd users on the same daemon.
	Namespace string
	// Image is the base image the workspace is provisioned from. Required:
	// a container with no rootfs has nothing to run.
	Image string
	// Runtime is the OCI runtime name, for example "io.containerd.runc.v2".
	// Empty uses containerd's default.
	Runtime string
	// Snapshotter selects the containerd snapshotter plugin. Empty uses
	// "overlayfs". Other snapshotters differ in commit semantics, so the
	// default is not assumed silently — it is croft's documented default.
	Snapshotter string
	// InitArgs is the long-lived command keeping the container up between
	// execs. Empty uses croft's `/bin/sh -c "sleep infinity"`, which needs a
	// shell in the image.
	InitArgs []string
	// Workdir is the workspace path inside the container; relative file paths
	// and commands resolve against it. Empty defaults to "/workspace".
	//
	// This governs the workspace root. sbx.SandboxSpec.Workdir is ignored by
	// Create, per that field's contract ("ignored by backends that infer their
	// own") — the container's root is fixed at construction, not per call.
	Workdir string
	// WriteChunkBytes bounds how much file content one WriteFile exec carries
	// before base64 expansion. Empty uses croft's default.
	WriteChunkBytes int
	// MaxOutputBytes caps each of an exec's stdout and stderr in memory,
	// applied per stream. Zero or negative means unbounded. Data transport
	// (ReadFile, diff readers) stays lossless regardless.
	MaxOutputBytes int
	// FIFODir is where the containerd *client* creates the stdio FIFOs for an
	// exec. containerd's default is root-owned, so an unprivileged caller must
	// set this to a directory it can write to.
	FIFODir string
	// Limits bounds the container's CPU, memory and process count. Nil means
	// unbounded. Fixed at construction: decide limits before New.
	Limits *Limits
	// Network is the workspace's network posture. "" or "none" keeps the
	// container's own unshared namespace — loopback only, no egress, DNS fails
	// fast, which is the default and the container lane's egress answer.
	// "host" is the explicit opt-in that shares the daemon host's network.
	// "broker" gives the container a veth with no default route and a
	// single-port host proxy that admits BrokerHosts.
	// Unknown values are rejected by croft at every generation-starting entry.
	Network string
	// BrokerHosts is the exact-host allowlist for Network: "broker". Empty
	// means deny all. It has no effect under another posture.
	BrokerHosts []string
	// WritableRoots is the container-path grant set. Empty keeps the
	// historical whole-workspace writable posture unless MaskWorkspace is
	// set; a non-empty set is the shaped container view (read-only workspace
	// plus these writable subtrees).
	WritableRoots []string
	// MaskWorkspace makes the workspace read-only without granting any
	// writable subtree. It is how an all-read-only grant declaration is
	// distinguished from the historical unshaped posture (empty
	// WritableRoots). It has no additional effect when WritableRoots is
	// non-empty.
	MaskWorkspace bool
	// Secrets are host files bind-mounted read-only into the container. A
	// token arrives as a file, never in argv or the process environment.
	Secrets []SecretMount
	// StagingDir is a host directory enabling argv-free writes: when set,
	// WriteFile stages through it and moves the payload into place inside the
	// container, so content appears in no argv. When unset, WriteFile keeps
	// croft's documented base64-in-argv posture.
	StagingDir string
}

// Limits bounds a container's resources, mapped onto the OCI runtime spec's
// Linux resources (cgroup v2 on a modern daemon). Zero-valued fields are left
// unset, so partial limits compose with the image and runtime defaults.
type Limits struct {
	// CPUShares is the relative CPU weight against other containers.
	CPUShares uint64
	// CPUQuota caps CPU bandwidth: at most CPUQuota microseconds of CPU time
	// per scheduling period (typically 100ms, so 50000 is half a CPU).
	CPUQuota int64
	// MemoryBytes is the cgroup v2 memory.max limit. Swap is accounted
	// separately and left at the runtime default.
	MemoryBytes int64
	// Pids caps the process count in the container's cgroup — the fork-bomb
	// fairness bound.
	Pids int64
}

// SecretMount binds a host file read-only into the container.
type SecretMount struct {
	// Source is an absolute path on the daemon host.
	Source string
	// Destination is the absolute in-container path.
	Destination string
}
