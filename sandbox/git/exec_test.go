package git

import (
	"context"
	"errors"
	"testing"

	trace "github.com/buchenberg/shepherd-kernel-go"
	sbx "github.com/buchenberg/shepherd-runtime-go/sandbox"
	"github.com/buchenberg/shepherd-runtime-go/sandbox/confinement"
	"github.com/buchenberg/shepherd-runtime-go/sandbox/containment"
)

// fakeConfiner is a containment.Backend double for the git backend's Exec tests.
type fakeConfiner struct {
	available bool
	probed    bool
	launched  bool
	result    containment.LaunchResult
	launchErr error
}

func (f *fakeConfiner) Name() string            { return "fake" }
func (f *fakeConfiner) EnforcementTier() string { return "test" }
func (f *fakeConfiner) Available() (bool, string) {
	if f.available {
		return true, "ok"
	}
	return false, "unavailable"
}
func (f *fakeConfiner) ProfileFor([]string, bool) string { return "[]" }
func (f *fakeConfiner) Probe(string, string, []string) error {
	f.probed = true
	return nil
}
func (f *fakeConfiner) Launch(string, containment.LaunchRequest) (containment.LaunchResult, error) {
	f.launched = true
	return f.result, f.launchErr
}

func TestGitSandbox_ExecRequiresBackend(t *testing.T) {
	sb := NewLocalGitSandbox(newTestRepo(t))
	if _, err := sb.Exec(context.Background(), sbx.ExecRequest{Command: "true"}); !errors.Is(err, sbx.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported without WithExec, got %v", err)
	}
}

func TestGitSandbox_ExecConfined(t *testing.T) {
	fake := &fakeConfiner{available: true, result: containment.LaunchResult{ExitCode: 7, Stdout: "hi"}}
	sb := NewLocalGitSandbox(newTestRepo(t), WithExec(fake, confinement.PlacementPaddock))

	res, err := sb.Exec(context.Background(), sbx.ExecRequest{Command: "echo", Args: []string{"hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 7 || res.Stdout != "hi" {
		t.Fatalf("result = %+v", res)
	}
	if !fake.probed || !fake.launched {
		t.Fatalf("paddock placement must probe then launch (probed=%v launched=%v)", fake.probed, fake.launched)
	}
}

func TestGitSandbox_ExecPaddockRefusesWithoutBackend(t *testing.T) {
	fake := &fakeConfiner{available: false}
	sb := NewLocalGitSandbox(newTestRepo(t), WithExec(fake, confinement.PlacementPaddock))

	if _, err := sb.Exec(context.Background(), sbx.ExecRequest{Command: "true"}); !errors.Is(err, containment.ErrPaddockNotEstablished) {
		t.Fatalf("want ErrPaddockNotEstablished, got %v", err)
	}
	if fake.launched {
		t.Fatal("must not launch when the paddock cannot be established")
	}
}

func TestGitSandbox_CapabilitiesHonest(t *testing.T) {
	repo := newTestRepo(t)

	def := NewLocalGitSandbox(repo).Capabilities()
	if def.Exec {
		t.Fatal("default sandbox must not claim Exec")
	}
	if def.Containment != trace.ContainUncontained {
		t.Fatalf("default containment = %q, want uncontained", def.Containment)
	}

	confined := NewLocalGitSandbox(repo, WithExec(&fakeConfiner{available: true}, confinement.PlacementPaddock)).Capabilities()
	if !confined.Exec || confined.Containment != trace.ContainContained {
		t.Fatalf("confined capabilities = %+v", confined)
	}

	advisory := NewLocalGitSandbox(repo, WithExec(&fakeConfiner{available: true}, confinement.PlacementAdvisory)).Capabilities()
	if !advisory.Exec || advisory.Containment != trace.ContainUncontained {
		t.Fatalf("advisory capabilities = %+v", advisory)
	}

	unavailable := NewLocalGitSandbox(repo, WithExec(&fakeConfiner{available: false}, confinement.PlacementAuto)).Capabilities()
	if !unavailable.Exec || unavailable.Containment != trace.ContainUncontained {
		t.Fatalf("unavailable-backend capabilities = %+v", unavailable)
	}
}

func TestGitSandbox_ExecRecordsAuthorityEvidence(t *testing.T) {
	fake := &fakeConfiner{available: true, result: containment.LaunchResult{ExitCode: 0}}
	sb := NewLocalGitSandbox(newTestRepo(t), WithExec(fake, confinement.PlacementPaddock))

	res, err := sb.Exec(context.Background(), sbx.ExecRequest{Command: "true", AuthorityMatch: "match-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.AuthorityEvidence == "" {
		t.Fatal("a confined run must carry authority evidence")
	}

	advisory := NewLocalGitSandbox(newTestRepo(t), WithExec(&fakeConfiner{available: true}, confinement.PlacementAdvisory))
	adv, err := advisory.Exec(context.Background(), sbx.ExecRequest{Command: "true", AuthorityMatch: "match-1"})
	if err != nil {
		t.Fatal(err)
	}
	if adv.AuthorityEvidence != "" {
		t.Fatal("an advisory run must not claim authority evidence")
	}
}

func TestGitSandbox_ExecMayLowersAndConfines(t *testing.T) {
	fake := &fakeConfiner{available: true, result: containment.LaunchResult{ExitCode: 0}}
	sb := NewLocalGitSandbox(newTestRepo(t), WithExec(fake, confinement.PlacementPaddock))

	if _, err := sb.ExecMay(context.Background(), sbx.ExecRequest{Command: "true"}, "ReadOnly", "m"); err != nil {
		t.Fatalf("ExecMay(ReadOnly): %v", err)
	}
	if !fake.launched {
		t.Fatal("paddock must launch the lowered run")
	}
	if _, err := sb.ExecMay(context.Background(), sbx.ExecRequest{Command: "true"}, "Standard", "m"); !errors.Is(err, confinement.ErrUnsupportedMayProfile) {
		t.Fatalf("unknown may= must refuse, got %v", err)
	}
}

func TestGitSandbox_ExecDeniedClassified(t *testing.T) {
	fake := &fakeConfiner{available: true, result: containment.LaunchResult{ExitCode: 1, Stderr: "touch: cannot touch '/x': Permission denied"}}
	sb := NewLocalGitSandbox(newTestRepo(t), WithExec(fake, confinement.PlacementPaddock))

	res, err := sb.Exec(context.Background(), sbx.ExecRequest{Command: "touch"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Denied {
		t.Fatal("a syscall denial must be flagged on the result")
	}
	if res.AuthorityEvidence == "" {
		t.Fatal("a denied run must still cite its authority evidence")
	}
}
