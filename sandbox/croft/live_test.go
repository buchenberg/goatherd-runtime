package croft

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	trace "github.com/buchenberg/goatherd-kernel"
	sbx "github.com/buchenberg/goatherd-runtime/sandbox"

	croftlib "github.com/buchenberg/goatherd-croft"
)

// Live verification against a real containerd daemon, reusing croft's own
// harness so one environment drives both suites.
//
// Skipped unless CROFT_CONTAINERD_ADDR is set, so `go test ./...` stays green
// on a machine that has not opted in:
//
//	sudo env CROFT_CONTAINERD_ADDR=/run/containerd/containerd.sock \
//	  go test ./sandbox/croft/ -run Live -v
//
// The namespace defaults to its own value rather than croft's, so an adapter
// run cannot collide with a concurrent croft live run on the same daemon.
const (
	liveEnvAddr        = "CROFT_CONTAINERD_ADDR"
	liveEnvImage       = "CROFT_CONTAINERD_IMAGE"
	liveEnvNamespace   = "CROFT_CONTAINERD_NAMESPACE"
	liveEnvSnapshotter = "CROFT_CONTAINERD_SNAPSHOTTER"
	liveEnvWorkdir     = "CROFT_CONTAINERD_WORKDIR"
	liveEnvTimeout     = "CROFT_CONTAINERD_TIMEOUT"

	liveDefaultImage       = "docker.io/alpine/git:latest"
	liveDefaultNamespace   = "croft-adapter-live"
	liveDefaultSnapshotter = "overlayfs"
	liveDefaultWorkdir     = "/workspace"
	liveDefaultTimeout     = 5 * time.Minute
)

type liveConfig struct {
	cfg     Config
	timeout time.Duration
}

// loadLiveConfig resolves the environment or skips the test. The address is the
// only required value: without it there is no daemon to talk to, and skipping
// keeps this file inert.
func loadLiveConfig(t *testing.T) liveConfig {
	t.Helper()

	addr := strings.TrimSpace(os.Getenv(liveEnvAddr))
	if addr == "" {
		t.Skipf("%s not set: skipping live containerd verification through the adapter", liveEnvAddr)
	}
	if err := probeSocket(addr); err != nil {
		t.Skipf("containerd socket %s is not reachable by this user (%v): skipping live verification. "+
			"containerd's default socket is root-only; a deployment granting it to a group "+
			"(or running as root) enables these tests", addr, err)
	}

	out := liveConfig{
		cfg: Config{
			Address:     addr,
			Image:       liveEnv(liveEnvImage, liveDefaultImage),
			Namespace:   liveEnv(liveEnvNamespace, liveDefaultNamespace),
			Snapshotter: liveEnv(liveEnvSnapshotter, liveDefaultSnapshotter),
			Workdir:     liveEnv(liveEnvWorkdir, liveDefaultWorkdir),
			// The client creates the stdio FIFOs itself and containerd's default
			// directory is root-owned; a temp dir keeps this independent of that.
			FIFODir: t.TempDir(),
		},
		timeout: liveDefaultTimeout,
	}
	if raw := strings.TrimSpace(os.Getenv(liveEnvTimeout)); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatalf("%s=%q is not a valid duration: %v", liveEnvTimeout, raw, err)
		}
		out.timeout = parsed
	}

	t.Logf("live adapter: addr=%s namespace=%s snapshotter=%s image=%s workdir=%s timeout=%s",
		out.cfg.Address, out.cfg.Namespace, out.cfg.Snapshotter, out.cfg.Image, out.cfg.Workdir, out.timeout)
	return out
}

func liveEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// probeSocket reports whether this process can actually reach the daemon, which
// is the thing the live tests depend on. It replaces an euid check: containerd's
// default socket is root-only, but a deployment that grants it to a group makes
// the tests runnable unprivileged — and croft's Config.FIFODir already removes
// the client-side need for root. Testing reachability directly means the suite
// runs wherever it can, and skips with an actionable message wherever it
// cannot, instead of guessing from the uid.
func probeSocket(addr string) error {
	conn, err := net.DialTimeout("unix", strings.TrimPrefix(addr, "unix://"), 5*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}

// liveSandbox builds an adapter through the public constructor — not the test
// seam — so the live path exercises the same Config lowering a host uses.
func liveSandbox(t *testing.T, lc liveConfig) *Sandbox {
	t.Helper()
	s, err := New(lc.cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestLiveSandbox_FullLifecycleThroughTheSeam(t *testing.T) {
	lc := loadLiveConfig(t)
	s := liveSandbox(t, lc)

	ctx, cancel := context.WithTimeout(context.Background(), lc.timeout)
	defer cancel()

	// Destroy on every path, including a failed Create: a leaked container in
	// the daemon's namespace is the failure mode this suite must never leave
	// behind.
	defer func() {
		dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer dcancel()
		if err := s.Destroy(dctx); err != nil {
			t.Errorf("teardown Destroy: %v", err)
		}
	}()

	if err := s.Create(ctx, sbx.SandboxSpec{Timeout: lc.timeout}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Capabilities must hold against a real daemon, not just a fake.
	if got := s.Capabilities(); !got.Isolated || got.Containment != trace.ContainContained {
		t.Errorf("Capabilities() = %+v; want Isolated and ContainContained", got)
	}

	// Exec inside the boundary.
	res, err := s.Exec(ctx, sbx.ExecRequest{Command: "sh", Args: []string{"-c", "echo hello-from-container"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("Exec exit %d: %s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "hello-from-container") {
		t.Errorf("stdout = %q, want the echoed marker", res.Stdout)
	}
	if res.Denied || res.AuthorityEvidence != "" {
		t.Error("a container run must not report paddock enforcement")
	}

	// File IO through the seam.
	if err := s.WriteFile(ctx, "greeting.txt", []byte("one"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := s.ReadFile(ctx, "greeting.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "one" {
		t.Errorf("ReadFile = %q, want %q", got, "one")
	}

	// Capture, then move the workspace on and roll back.
	state, err := s.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if state.Backend != BackendName || state.Revision == "" {
		t.Fatalf("captured state is malformed: %+v", state)
	}
	if _, err := state.Digest(); err != nil {
		t.Fatalf("state digest: %v", err)
	}

	if err := s.WriteFile(ctx, "greeting.txt", []byte("two"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	diff, files, err := s.Diff(ctx, state, 0)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(files) == 0 {
		t.Errorf("Diff reported no changed files; diff was:\n%s", diff)
	}
	if !strings.Contains(strings.Join(files, ","), "greeting.txt") {
		t.Errorf("Diff files = %v, want greeting.txt among them", files)
	}

	if err := s.Apply(ctx, state); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	back, err := s.ReadFile(ctx, "greeting.txt")
	if err != nil {
		t.Fatalf("ReadFile after Apply: %v", err)
	}
	if string(back) != "one" {
		t.Errorf("after Apply the file is %q, want the captured %q", back, "one")
	}
}

// TestLiveSandbox_NetworkIsLoopbackOnly pins the posture the container lane
// relies on for egress: croft's default unshares the network namespace, so a
// container session is loopback-only without any paddock involvement. This is
// the answer to yaah-kit's open question about how the container lane coexists
// with the egress vocabulary — the netns is the enforcement.
func TestLiveSandbox_NetworkIsLoopbackOnly(t *testing.T) {
	lc := loadLiveConfig(t)
	if lc.cfg.Network != "" && lc.cfg.Network != "none" {
		t.Skipf("CROFT_CONTAINERD_* configured Network=%q; this test pins the isolated default", lc.cfg.Network)
	}
	s := liveSandbox(t, lc)

	ctx, cancel := context.WithTimeout(context.Background(), lc.timeout)
	defer cancel()
	defer func() {
		dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer dcancel()
		_ = s.Destroy(dctx)
	}()

	if err := s.Create(ctx, sbx.SandboxSpec{Timeout: lc.timeout}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Loopback answers; an external name does not resolve.
	if res, err := s.Exec(ctx, sbx.ExecRequest{
		Command: "sh",
		Args:    []string{"-c", "wget -q -O- --timeout=5 http://127.0.0.1/ 2>&1; echo rc=$?"},
	}); err == nil {
		t.Logf("loopback probe: %s", strings.TrimSpace(res.Stdout+res.Stderr))
	}
	res, err := s.Exec(ctx, sbx.ExecRequest{
		Command: "sh",
		Args:    []string{"-c", "getent hosts example.com >/dev/null 2>&1; echo $?"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(res.Stdout) == "0" {
		t.Error("DNS resolved inside a workspace that should be loopback-only: the netns is not unshared")
	}
}

// TestLiveSandbox_DeltaConflictFailsClosed verifies settlement's apply verb
// against a real daemon: a divergent edit to the same path must conflict and
// leave the workspace untouched, never synthesize a merge.
func TestLiveSandbox_DeltaConflictFailsClosed(t *testing.T) {
	lc := loadLiveConfig(t)
	base := liveSandbox(t, lc)

	ctx, cancel := context.WithTimeout(context.Background(), lc.timeout)
	defer cancel()
	defer func() {
		dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer dcancel()
		_ = base.Destroy(dctx)
	}()

	if err := base.Create(ctx, sbx.SandboxSpec{Timeout: lc.timeout}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := base.WriteFile(ctx, "shared.txt", []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Establish a merge base so ApplyDelta has something to three-way against.
	seed, err := base.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if err := base.Apply(ctx, seed); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := base.WriteFile(ctx, "shared.txt", []byte("branch\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	delta, err := base.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// The workspace now diverges on the same path.
	if err := base.WriteFile(ctx, "shared.txt", []byte("trunk\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before, err := base.ReadFile(ctx, "shared.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	err = base.ApplyDelta(ctx, delta)
	if err == nil {
		t.Fatal("ApplyDelta of a divergent edit to the same path must conflict")
	}
	if !errors.Is(err, croftlib.ErrDeltaConflict) {
		t.Errorf("errors.Is(err, croft.ErrDeltaConflict) = false; settlement cannot classify this refusal: %v", err)
	}

	after, err := base.ReadFile(ctx, "shared.txt")
	if err != nil {
		t.Fatalf("ReadFile after conflict: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("a refused merge must leave the workspace untouched: %q became %q", before, after)
	}
}
