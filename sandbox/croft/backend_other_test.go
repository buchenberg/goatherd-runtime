//go:build !linux

package croft

import (
	"errors"
	"strings"
	"testing"

	sbx "github.com/buchenberg/goatherd-runtime/sandbox"
)

// These tests run on the platforms CI builds but cannot run containers on —
// Windows and macOS in the runtime's three-OS matrix. They pin the stub's
// honesty: the package must compile, expose the same surface, and refuse
// clearly, rather than either failing to build or pretending to work.

func TestStub_NewRefuses(t *testing.T) {
	sb, err := New(Config{Image: "docker.io/alpine/git:latest"})
	if err == nil {
		t.Fatal("New must refuse on a non-Linux platform")
	}
	if sb != nil {
		t.Errorf("a refused New must not hand back a sandbox, got %v", sb)
	}
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Errorf("errors.Is(err, ErrUnsupportedPlatform) = false, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Linux") {
		t.Errorf("the refusal must name the platform requirement, got: %v", err)
	}
}

func TestStub_BackendIsNotRegistered(t *testing.T) {
	// No init() registers the backend here, so the registry path fails first
	// and more informatively than New would: it names what *is* available.
	// This is the honest outcome — a config naming "containerd" on Windows is
	// a configuration error, and the registry says so.
	_, err := sbx.OpenSandbox(BackendName, Config{Image: "docker.io/alpine/git:latest"})
	if err == nil {
		t.Fatal("OpenSandbox must not resolve the containerd backend on a non-Linux platform")
	}
	var unknown *sbx.UnknownSandboxBackendError
	if !errors.As(err, &unknown) {
		t.Errorf("want an UnknownSandboxBackendError, got %T: %v", err, err)
	}
	for _, name := range sbx.SandboxBackends() {
		if name == BackendName {
			t.Errorf("%q must not be registered on this platform", BackendName)
		}
	}
}

func TestStub_SurfaceMatchesTheLinuxBuild(t *testing.T) {
	// The point of the stub is that a host can carry containerd settings in one
	// config type and compile on every platform. If a field is added to the
	// Linux Config and not here — or vice versa — a host's Windows build breaks
	// in a way that has nothing to do with containers. Config lives in an
	// untagged file precisely so that cannot drift; this test names every field
	// so removing one is a deliberate, visible act.
	cfg := Config{
		Address:         "/run/containerd/containerd.sock",
		Namespace:       "yaah",
		Image:           "docker.io/alpine/git:latest",
		Runtime:         "io.containerd.runc.v2",
		Snapshotter:     "overlayfs",
		InitArgs:        []string{"/bin/sh"},
		Workdir:         "/workspace",
		WriteChunkBytes: 4096,
		MaxOutputBytes:  1 << 20,
		FIFODir:         "/tmp/fifo",
		Limits:          &Limits{CPUShares: 1024, CPUQuota: 50000, MemoryBytes: 1 << 30, Pids: 256},
		Network:         "none",
		Secrets:         []SecretMount{{Source: "/host/token", Destination: "/run/token"}},
		StagingDir:      "/tmp/staging",
	}
	if cfg.Image == "" || cfg.Limits == nil || len(cfg.Secrets) != 1 {
		t.Error("the Config surface did not survive compilation as expected")
	}
	if BackendName != "containerd" {
		t.Errorf("BackendName = %q, want %q", BackendName, "containerd")
	}

	// The stub type must still answer Backend(), so diagnostics can name what a
	// caller asked for even where it cannot be provided.
	s := &Sandbox{cfg: cfg}
	if got := s.Backend(); got != BackendName {
		t.Errorf("Backend() = %q, want %q", got, BackendName)
	}

	// EffectChanger is part of the same untagged surface, and referencing the
	// type here is the pin: a host must be able to name the capability it probes
	// for on every platform it builds for, even where nothing implements it.
	// Were the interface moved into a Linux-only file, this test would stop
	// compiling on Windows and macOS — exactly the drift it exists to catch.
	//
	// Deliberately a declaration and not a comparison. Converting nil to an
	// interface type always yields a nil interface, so `EffectChanger(nil) != nil`
	// is a compile-time constant whose branch could never run, which would read
	// as a runtime assertion this test does not make.
	var _ EffectChanger
}
