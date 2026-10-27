//go:build linux

package croft

import (
	"errors"
	"strings"
	"testing"

	sbx "github.com/buchenberg/goatherd-runtime/sandbox"
)

func TestRegistry_OpenSandboxResolves(t *testing.T) {
	// Importing this module must make the backend selectable by name, the
	// sandbox/git self-registration pattern: config can name a backend, only an
	// import can provide one.
	got, err := sbx.OpenSandbox(BackendName, Config{Image: testImage})
	if err != nil {
		t.Fatalf("OpenSandbox(%q): %v", BackendName, err)
	}
	if got == nil {
		t.Fatal("OpenSandbox returned a nil sandbox without an error")
	}
	if got.Backend() != BackendName {
		t.Errorf("Backend() = %q, want %q", got.Backend(), BackendName)
	}
	// The registry hands back the interface; the concrete adapter must still
	// carry the optional surfaces callers find by type assertion.
	if _, ok := got.(sbx.DeltaApplier); !ok {
		t.Error("the resolved sandbox must be a DeltaApplier")
	}
}

func TestRegistry_BackendIsListed(t *testing.T) {
	// SandboxBackends backs config diagnostics and doctor output, so a
	// registered backend that does not appear there is a broken diagnostic.
	found := false
	for _, name := range sbx.SandboxBackends() {
		if name == BackendName {
			found = true
		}
	}
	if !found {
		t.Errorf("%q missing from SandboxBackends() = %v", BackendName, sbx.SandboxBackends())
	}
}

func TestRegistry_RejectsWrongConfigType(t *testing.T) {
	// The registry convention: cfg is opaque to the kernel, so a backend must
	// reject anything else loudly and a mismatch is a startup error, not a
	// silent default.
	type unrelated struct{ Path string }

	_, err := sbx.OpenSandbox(BackendName, unrelated{Path: "/tmp"})
	if err == nil {
		t.Fatal("OpenSandbox must reject a config type this backend does not accept")
	}
	if !strings.Contains(err.Error(), "croft.Config") || !strings.Contains(err.Error(), "unrelated") {
		t.Errorf("error must name both the wanted and the got type, got: %v", err)
	}
}

func TestRegistry_RejectsNilConfig(t *testing.T) {
	// Mirrors the git backend's wording: a backend cannot be chosen by
	// omission, because a nil config would otherwise mean "whatever the
	// default is" and silently pick a container image nobody named.
	_, err := sbx.OpenSandbox(BackendName, nil)
	if err == nil {
		t.Fatal("OpenSandbox must reject a nil config")
	}
	if !strings.Contains(err.Error(), "nil is not one") {
		t.Errorf("error should explain that omission is not a choice, got: %v", err)
	}
}

func TestRegistry_RejectsMissingImage(t *testing.T) {
	// Surfaced at the seam rather than as a daemon failure at Create, so a
	// config typo is a startup error naming the field.
	_, err := sbx.OpenSandbox(BackendName, Config{Address: "/run/containerd/containerd.sock"})
	if err == nil {
		t.Fatal("OpenSandbox must reject a config with no Image")
	}
	if !strings.Contains(err.Error(), "Config.Image") {
		t.Errorf("error must name the missing field, got: %v", err)
	}
}

func TestRegistry_UnknownBackendStillFailsClosed(t *testing.T) {
	// Not this backend's behaviour, but the seam it registers into: an
	// unregistered name must name the available backends rather than fail
	// opaquely. Guards against this module accidentally shadowing the
	// registry's error path.
	_, err := sbx.OpenSandbox("nonexistent-backend", Config{Image: testImage})
	if err == nil {
		t.Fatal("an unregistered backend must not resolve")
	}
	var unknown *sbx.UnknownSandboxBackendError
	if !errors.As(err, &unknown) {
		t.Errorf("error should be an UnknownSandboxBackendError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), BackendName) {
		t.Errorf("error must list %q among the available backends, got: %v", BackendName, err)
	}
}
