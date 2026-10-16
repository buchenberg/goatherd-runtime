//go:build linux

package landlock

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	trace "github.com/buchenberg/shepherd-kernel-go"
	shepherd "github.com/buchenberg/shepherd-runtime-go"
	"github.com/buchenberg/shepherd-runtime-go/run"
	"github.com/buchenberg/shepherd-runtime-go/sandbox"
	"github.com/buchenberg/shepherd-runtime-go/sandbox/confinement"
	"github.com/buchenberg/shepherd-runtime-go/sandbox/containment"
	"github.com/buchenberg/shepherd-runtime-go/sandbox/egress"
	"github.com/buchenberg/shepherd-runtime-go/sandbox/git"
)

// The confined connect helpers' sentinels, dispatched by TestMain.
const (
	e2eConnectSentinel = "__e2e_connect__"
	e2eDirectSentinel  = "__e2e_direct__"
)

// TestE2E_ReversibleBrokerEgress exercises the whole network-egress stack end to
// end: a reversible run whose isolated worktree executes confined commands that
// (a) reach an allowlisted host only *through* the egress broker, and (b) cannot
// bypass it — a direct connect to the same host is refused by the Landlock
// network pin, which admits only the broker's port.
func TestE2E_ReversibleBrokerEgress(t *testing.T) {
	b := requireLandlock(t)
	if !b.SupportsNetwork(containment.NetBroker) {
		t.Skip("kernel lacks Landlock network control (ABI < 4)")
	}
	if _, err := os.Executable(); err != nil {
		t.Skipf("cannot locate own executable: %v", err)
	}
	exe, _ := os.Executable()

	// A loopback echo upstream the broker forwards to.
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	echoPort := echo.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()

	// The egress broker: allowlist loopback, so the CONNECT target is admitted.
	broker := egress.NewBroker(egress.AllowHosts("127.0.0.1"))
	brokerPort, err := broker.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Stop()

	// Repo + a store-backed scope manager + a parent scope.
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "commit", "--allow-empty", "-m", "init")
	store, err := trace.NewSQLiteTraceStore(filepath.Join(t.TempDir(), "trace.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	mgr := shepherd.NewScopeManager(store)
	parent, err := mgr.Create("e2e:main", git.NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatal(err)
	}

	worktree := filepath.Join(t.TempDir(), "wt")
	spec := containment.ConfinementSpec{
		WritableRoots: []string{worktree},
		Network:       containment.NetworkPolicy{Mode: containment.NetBroker, BrokerPort: brokerPort},
	}
	proxy := net.JoinHostPort("127.0.0.1", strconv.Itoa(brokerPort))
	echoAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(echoPort))

	var brokerCode, directCode int
	_, err = run.Reversible(context.Background(), mgr, parent, run.Options{
		OwnerID:     "e2e:run",
		Placement:   confinement.PlacementPaddock,
		Backend:     b,
		Confinement: &spec,
		NewSandbox: func(string) (sandbox.Sandbox, error) {
			return git.NewWorktreeSandbox(repo, worktree, git.WithExec(b, confinement.PlacementPaddock)), nil
		},
	}, func(_ context.Context, rc *run.RunContext) (run.Result, error) {
		// (a) Through the broker: admitted by the pin, allowed by the broker.
		res, err := rc.Capability.LaunchConfined(rc.Confinement, containment.LaunchRequest{
			Command:     []string{exe, e2eConnectSentinel, proxy, echoAddr},
			WorkingRoot: rc.Capability.WorkingRoot,
		})
		if err != nil {
			return run.Result{}, fmt.Errorf("broker egress: %w", err)
		}
		brokerCode = res.ExitCode

		// (b) Direct to the same host, bypassing the broker — the pin refuses.
		res, err = rc.Capability.LaunchConfined(rc.Confinement, containment.LaunchRequest{
			Command:     []string{exe, e2eDirectSentinel, echoAddr},
			WorkingRoot: rc.Capability.WorkingRoot,
		})
		if err != nil {
			return run.Result{}, fmt.Errorf("direct egress: %w", err)
		}
		directCode = res.ExitCode
		return run.Result{}, nil
	})
	if err != nil {
		t.Fatalf("Reversible: %v", err)
	}

	if brokerCode != 0 {
		t.Fatalf("through-broker connect rc = %d, want 0 (broker admitted)", brokerCode)
	}
	if directCode != 1 {
		t.Fatalf("direct connect rc = %d, want 1 (refused by the network pin)", directCode)
	}

	allowed := false
	for _, d := range broker.Decisions() {
		if d.Verdict == egress.VerdictAllow && d.Host == "127.0.0.1" {
			allowed = true
		}
	}
	if !allowed {
		t.Fatalf("the broker recorded no allow decision: %+v", broker.Decisions())
	}
}

// The confined connect helpers, dispatched by TestMain. They do NOT confine — the
// landlock trampoline already confined the process before exec'ing them.

// e2eConnect does an HTTP CONNECT through proxy to target, returning 0 on a 200,
// 3 on a 403, 1 if the connect was refused at the syscall, 2 otherwise.
func e2eConnect(proxy, target string) int {
	conn, err := net.DialTimeout("tcp", proxy, 3*time.Second)
	if err != nil {
		return directCodeOf(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\n\r\n", target)
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return 2
	}
	switch {
	case strings.Contains(line, "200"):
		return 0
	case strings.Contains(line, "403"):
		return 3
	default:
		return 2
	}
}

// e2eDirect connects directly to addr, returning 0 on success, 1 if refused at
// the syscall, 2 otherwise.
func e2eDirect(addr string) int {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return directCodeOf(err)
	}
	_ = conn.Close()
	return 0
}

func directCodeOf(err error) int {
	if isSyscallDenied(err) {
		return 1
	}
	return 2
}
