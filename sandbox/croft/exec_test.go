//go:build linux

package croft

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sbx "github.com/buchenberg/goatherd-runtime/sandbox"
	"github.com/buchenberg/goatherd-runtime/sandbox/confinement"
	"github.com/buchenberg/goatherd-runtime/sandbox/containment"

	croftlib "github.com/buchenberg/goatherd-croft"
)

func TestSandbox_ExecBoundedByDefaultCeiling(t *testing.T) {
	// The Sandbox contract: a request carrying no Timeout still runs under a
	// backend ceiling, so a caller passing context.Background() cannot hang the
	// session on a wedged daemon.
	old := execCeiling
	execCeiling = 300 * time.Millisecond
	t.Cleanup(func() { execCeiling = old })

	s, fake := newTestSandbox(t)
	start := time.Now()
	if _, err := s.Exec(context.Background(), sbx.ExecRequest{Command: "true"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if fake.lastExecDeadline.IsZero() {
		t.Fatal("Exec must bound the context when the request carries no Timeout")
	}
	if got := fake.lastExecDeadline.Sub(start); got < 200*time.Millisecond || got > 500*time.Millisecond {
		t.Errorf("deadline was %v from now, want roughly the %v ceiling", got, execCeiling)
	}
	if fake.lastExecReq.Timeout != execCeiling {
		t.Errorf("croft received Timeout %v, want the ceiling %v", fake.lastExecReq.Timeout, execCeiling)
	}
}

func TestSandbox_ExecHonorsExplicitTimeout(t *testing.T) {
	// An explicit Timeout is the caller's contract and is not clamped to the
	// ceiling — including when it exceeds it.
	old := execCeiling
	execCeiling = time.Second
	t.Cleanup(func() { execCeiling = old })

	s, fake := newTestSandbox(t)
	start := time.Now()
	want := 30 * time.Second
	if _, err := s.Exec(context.Background(), sbx.ExecRequest{Command: "true", Timeout: want}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if fake.lastExecReq.Timeout != want {
		t.Errorf("croft received Timeout %v, want the caller's %v (must not be clamped to the %v ceiling)",
			fake.lastExecReq.Timeout, want, execCeiling)
	}
	if got := fake.lastExecDeadline.Sub(start); got < 29*time.Second || got > 31*time.Second {
		t.Errorf("deadline was %v out, want roughly %v", got, want)
	}
}

func TestSandbox_ExecMapsRequest(t *testing.T) {
	s, fake := newTestSandbox(t)
	req := sbx.ExecRequest{
		Command: "go",
		Args:    []string{"build", "./..."},
		Cwd:     "/work/pkg",
		Env:     map[string]string{"GOFLAGS": "-mod=mod"},
		Stdin:   []byte("piped input"),
		Timeout: 5 * time.Second,
	}
	if _, err := s.Exec(context.Background(), req); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	got := fake.lastExecReq
	if got.Command != req.Command || strings.Join(got.Args, " ") != "build ./..." {
		t.Errorf("command not forwarded: %q %v", got.Command, got.Args)
	}
	if got.Cwd != req.Cwd {
		t.Errorf("Cwd = %q, want %q", got.Cwd, req.Cwd)
	}
	if got.Env["GOFLAGS"] != "-mod=mod" {
		t.Errorf("Env not forwarded: %v", got.Env)
	}
	if string(got.Stdin) != "piped input" {
		t.Errorf("Stdin = %q, want %q", got.Stdin, "piped input")
	}
}

func TestSandbox_ExecMapsResult(t *testing.T) {
	s, fake := newTestSandbox(t)
	fake.execFn = func(context.Context, croftlib.ExecRequest) (croftlib.ExecResult, error) {
		return croftlib.ExecResult{
			ExitCode: 3,
			Stdout:   "out",
			Stderr:   "err",
			Duration: 250 * time.Millisecond,
		}, nil
	}

	got, err := s.Exec(context.Background(), sbx.ExecRequest{Command: "false"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got.ExitCode != 3 || got.Stdout != "out" || got.Stderr != "err" || got.Duration != 250*time.Millisecond {
		t.Errorf("result not mapped faithfully: %+v", got)
	}
}

func TestSandbox_ExecNeverClaimsEnforcement(t *testing.T) {
	// ExecResult.Denied and AuthorityEvidence are meaningful only for a
	// paddocked run. This backend enforces nothing at the syscall level, so
	// reporting either would claim enforcement it does not do — including on a
	// run that failed, where a caller might otherwise read the failure as a
	// denial.
	s, fake := newTestSandbox(t)
	fake.execFn = func(context.Context, croftlib.ExecRequest) (croftlib.ExecResult, error) {
		return croftlib.ExecResult{ExitCode: 1, Stderr: "permission denied"}, nil
	}

	got, err := s.Exec(context.Background(), sbx.ExecRequest{Command: "sh"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got.Denied {
		t.Error("Denied must stay false: the container boundary is not a syscall paddock")
	}
	if got.AuthorityEvidence != "" {
		t.Errorf("AuthorityEvidence must stay empty, got %q", got.AuthorityEvidence)
	}
}

func TestSandbox_ExecRefusesAuthorityItCannotEnforce(t *testing.T) {
	// Fail closed rather than silently satisfy a grant-shaped request with the
	// coarse container boundary. Provisional pending croft-ku0; see Exec.
	s, _ := newTestSandbox(t)
	spec := &confinement.ConfinementSpec{}

	cases := []struct {
		name string
		req  sbx.ExecRequest
		want string
	}{
		{"confinement spec", sbx.ExecRequest{Command: "true", Confinement: spec}, "read-only confinement"},
		{"paddock placement", sbx.ExecRequest{Command: "true", Placement: containment.PlacementPaddock}, "paddock"},
		{"authority match", sbx.ExecRequest{Command: "true", AuthorityMatch: "sha256:deadbeef"}, "authority-surface"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Exec(context.Background(), tc.req)
			if err == nil {
				t.Fatal("Exec must refuse an authority request the container tier cannot honor")
			}
			if !errors.Is(err, sbx.ErrUnsupported) {
				t.Errorf("errors.Is(err, sandbox.ErrUnsupported) = false; a caller probing capabilities cannot classify this: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error must mention %q, got: %v", tc.want, err)
			}
		})
	}
}

func TestSandbox_ExecRefusesUnknownPlacement(t *testing.T) {
	// A placement outside the vocabulary must be refused, not treated as the
	// permissive default. containment.ResolvePlacement rejects unknown values,
	// so accepting one here would make this backend laxer than the layer that
	// produces placements — and a capitalisation typo would silently downgrade
	// a request for enforcement into a request for none.
	s, _ := newTestSandbox(t)

	for _, p := range []containment.Placement{"Paddock", "PADDOCK", "paddok", "confined", "sandbox"} {
		t.Run(string(p), func(t *testing.T) {
			_, err := s.Exec(context.Background(), sbx.ExecRequest{Command: "true", Placement: p})
			if err == nil {
				t.Fatalf("placement %q is not in the vocabulary and must be refused", p)
			}
			// A caller bug, not an unsupported feature: it must not read as
			// ErrUnsupported, or a capability probe would treat a typo as "this
			// backend lacks a feature" and move on.
			if errors.Is(err, sbx.ErrUnsupported) {
				t.Errorf("an unknown placement is invalid, not unsupported: %v", err)
			}
			if !strings.Contains(err.Error(), "unknown placement") {
				t.Errorf("error must say the value is unknown, got: %v", err)
			}
			// Name the valid values so the typo is fixable from the message.
			for _, valid := range []string{"paddock", "advisory", "auto"} {
				if !strings.Contains(err.Error(), valid) {
					t.Errorf("error must list %q as valid, got: %v", valid, err)
				}
			}
		})
	}
}

func TestSandbox_ExecAcceptsNonPaddockPlacements(t *testing.T) {
	// Advisory, auto and empty placements ask for no syscall-tier enforcement,
	// so the container boundary is an honest answer and Exec must not refuse.
	s, _ := newTestSandbox(t)
	for _, p := range []containment.Placement{"", containment.PlacementAdvisory, containment.PlacementAuto} {
		t.Run(string(p)+"(or empty)", func(t *testing.T) {
			if _, err := s.Exec(context.Background(), sbx.ExecRequest{Command: "true", Placement: p}); err != nil {
				t.Errorf("placement %q must be accepted, got: %v", p, err)
			}
		})
	}
}

func TestSandbox_ExecPropagatesError(t *testing.T) {
	s, fake := newTestSandbox(t)
	fake.execErr = errors.New("task vanished")
	if _, err := s.Exec(context.Background(), sbx.ExecRequest{Command: "true"}); err == nil {
		t.Fatal("Exec must propagate a croft failure")
	}
}

func TestSandbox_ExecDeadlineSurfacesAsContextError(t *testing.T) {
	// The contract is explicit: a run the deadline kills surfaces as a
	// context-deadline error, never as a result with a negative exit code. A
	// caller must be able to distinguish "the command failed" from "we gave up
	// waiting" without inspecting an exit status.
	old := execCeiling
	execCeiling = 50 * time.Millisecond
	t.Cleanup(func() { execCeiling = old })

	s, fake := newTestSandbox(t)
	fake.execFn = func(ctx context.Context, _ croftlib.ExecRequest) (croftlib.ExecResult, error) {
		<-ctx.Done()
		return croftlib.ExecResult{}, ctx.Err()
	}

	_, err := s.Exec(context.Background(), sbx.ExecRequest{Command: "sleep", Args: []string{"60"}})
	if err == nil {
		t.Fatal("a deadline-killed run must return an error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, context.DeadlineExceeded) = false; wrapping lost the cause: %v", err)
	}
}

func TestSandbox_ExecAcceptsMatchingConfiguredAuthority(t *testing.T) {
	fake := newFakeCroft(testImage)
	s := newWithInner(Config{
		Image:         testImage,
		WritableRoots: []string{"/workspace/src"},
		Network:       "broker",
		BrokerHosts:   []string{"api.example.com"},
	}, fake)

	spec := &confinement.ConfinementSpec{
		WritableRoots: []string{"/workspace/src"},
		Network: confinement.NetworkPolicy{
			Mode:         confinement.NetBroker,
			BrokerPort:   3128,
			AllowedHosts: []string{"api.example.com"},
		},
	}
	if _, err := s.Exec(context.Background(), sbx.ExecRequest{Command: "true", Confinement: spec}); err != nil {
		t.Fatalf("Exec with a matching confinement spec: %v", err)
	}

	// A different root or posture is a mismatch, not a silent reshape.
	spec.WritableRoots = []string{"/workspace/other"}
	if _, err := s.Exec(context.Background(), sbx.ExecRequest{Command: "true", Confinement: spec}); !errors.Is(err, sbx.ErrUnsupported) {
		t.Fatalf("mismatched roots must be unsupported, got %v", err)
	}
}

func TestSandbox_ExecAcceptsReadOnlyMask(t *testing.T) {
	// MaskWorkspace is how an all-read-only declaration is distinguished
	// from the unshaped legacy posture. A matching empty-root spec is
	// enforceable; the same spec against an unshaped config is not.
	fake := newFakeCroft(testImage)
	s := newWithInner(Config{Image: testImage, MaskWorkspace: true}, fake)
	spec := &confinement.ConfinementSpec{}

	if _, err := s.Exec(context.Background(), sbx.ExecRequest{Command: "true", Confinement: spec}); err != nil {
		t.Fatalf("Exec with a matching read-only mask: %v", err)
	}

	unshaped := newWithInner(Config{Image: testImage}, fake)
	if _, err := unshaped.Exec(context.Background(), sbx.ExecRequest{Command: "true", Confinement: spec}); !errors.Is(err, sbx.ErrUnsupported) {
		t.Fatalf("a read-only spec against an unshaped container must be unsupported, got %v", err)
	}
}
