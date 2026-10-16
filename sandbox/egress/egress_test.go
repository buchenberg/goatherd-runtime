package egress

import (
	"bufio"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/buchenberg/shepherd-runtime-go/sandbox/containment"
)

func TestHostAtom_Matches(t *testing.T) {
	cases := []struct {
		op    string
		value any
		host  string
		want  bool
	}{
		{"exact", "api.example.com", "api.example.com", true},
		{"exact", "api.example.com", "other.example.com", false},
		{"in", []string{"a.com", "b.com"}, "b.com", true},
		{"in", []string{"a.com"}, "c.com", false},
		{"contains", "example", "api.example.com", true},
		{"matches", `.*\.example\.com`, "api.example.com", true},
		{"matches", `.*\.example\.com`, "example.com", false},
	}
	for _, tc := range cases {
		got, err := HostAtom{Op: tc.op, Value: tc.value}.Matches(tc.host)
		if err != nil {
			t.Fatalf("%s/%v: %v", tc.op, tc.value, err)
		}
		if got != tc.want {
			t.Errorf("%s(%q) = %v, want %v", tc.op, tc.host, got, tc.want)
		}
	}
	if _, err := (HostAtom{Op: "bogus"}).Matches("x"); err == nil {
		t.Fatal("an unknown op must error, never allow")
	}
}

func TestEgressPolicy_Decide(t *testing.T) {
	p := AllowHosts("api.example.com")
	if d, _ := p.Decide("api.example.com", 443); d.Verdict != VerdictAllow || d.Kind != "broker:connect-allowed" {
		t.Fatalf("allowed host: %+v", d)
	}
	if d, _ := p.Decide("evil.example.com", 443); d.Verdict != VerdictDeny || d.Kind != "broker:connect-denied" {
		t.Fatalf("default-deny: %+v", d)
	}
	esc := PolicyFromGrants(HostGrant{Positive: []HostAtom{{Op: "exact", Value: "x.com"}}, Verdict: VerdictEscalate})
	if d, _ := esc.Decide("x.com", 443); d.Verdict != VerdictEscalate {
		t.Fatalf("escalate: %+v", d)
	}
}

func TestEgressPolicy_DenyWins(t *testing.T) {
	g := HostGrant{
		Positive: []HostAtom{{Op: "contains", Value: "example.com"}},
		Negative: []HostAtom{{Op: "exact", Value: "blocked.example.com"}},
	}
	p := PolicyFromGrants(g)
	if d, _ := p.Decide("ok.example.com", 443); d.Verdict != VerdictAllow {
		t.Fatalf("positive should admit: %+v", d)
	}
	if d, _ := p.Decide("blocked.example.com", 443); d.Verdict != VerdictDeny {
		t.Fatalf("negative must exclude: %+v", d)
	}
	// Case- and trailing-dot-insensitive: a client must not bypass the negative
	// rule by varying the host's case or appending a root dot.
	for _, host := range []string{"BLOCKED.example.com", "blocked.example.com.", "Blocked.Example.COM."} {
		if d, _ := p.Decide(host, 443); d.Verdict != VerdictDeny {
			t.Fatalf("host %q must be denied (normalized), got %+v", host, d)
		}
	}
}

func TestBroker_AllowsAndDenies(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	_, echoPort, _ := net.SplitHostPort(echo.Addr().String())

	broker := NewBroker(AllowHosts("127.0.0.1"))
	port, err := broker.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Stop()

	// Allowed: CONNECT to the loopback echo → 200, then the payload round-trips.
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(conn, "CONNECT 127.0.0.1:"+echoPort+" HTTP/1.1\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	if status, err := br.ReadString('\n'); err != nil || !strings.Contains(status, "200") {
		t.Fatalf("allowed CONNECT = %q, %v", status, err)
	}
	if _, err := br.ReadString('\n'); err != nil { // the blank line ending the head
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo = %q", buf)
	}
	_ = conn.Close()

	// Denied: CONNECT to an unlisted host → 403.
	conn2, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn2.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(conn2, "CONNECT denied.test:80 HTTP/1.1\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	status2, err := bufio.NewReader(conn2).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status2, "403") {
		t.Fatalf("denied CONNECT = %q", status2)
	}
	_ = conn2.Close()

	decs := broker.Decisions()
	if len(decs) != 2 {
		t.Fatalf("decisions = %d, want 2 (%+v)", len(decs), decs)
	}
	if decs[0].Verdict != VerdictAllow || decs[1].Verdict != VerdictDeny {
		t.Fatalf("decisions = %+v", decs)
	}
	if decs[1].Host != "denied.test" || decs[1].Port != 80 {
		t.Fatalf("denied decision = %+v", decs[1])
	}
}

func TestBroker_BadRequest(t *testing.T) {
	broker := NewBroker(AllowHosts("x"))
	port, err := broker.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Stop()

	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\n\r\n")
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "400") {
		t.Fatalf("bad request = %q", status)
	}
}

func TestPolicyFromNetwork(t *testing.T) {
	p, err := PolicyFromNetwork(containment.NetworkPolicy{Mode: containment.NetBroker, AllowedHosts: []string{"api.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := p.Decide("api.example.com", 443); d.Verdict != VerdictAllow {
		t.Fatalf("allowed: %+v", d)
	}
	if d, _ := p.Decide("evil.example.com", 443); d.Verdict != VerdictDeny {
		t.Fatalf("default-deny: %+v", d)
	}
	if _, err := PolicyFromNetwork(containment.NetworkPolicy{Mode: containment.NetAllowAll}); err == nil {
		t.Fatal("a non-broker mode must be refused")
	}
}
