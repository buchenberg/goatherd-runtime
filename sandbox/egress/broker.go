package egress

import (
	"bufio"
	"io"
	"net"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// connectRe parses the CONNECT request line: CONNECT host:port HTTP/1.1.
var connectRe = regexp.MustCompile(`(?i)^CONNECT\s+([^:\s]+):(\d+)\s+HTTP`)

// Broker is a loopback-bound HTTP-CONNECT proxy enforcing an EgressPolicy.
//
// Forwards allowed connections (200 + bidirectional splice); refuses denied and
// escalate-unbuilt ones (403); records every verdict. Start it, read Port(),
// point the jail's network pin at it, Stop it on scope close.
type Broker struct {
	policy     EgressPolicy
	onDecision func(EgressDecision)

	mu        sync.Mutex
	decisions []EgressDecision
	ln        net.Listener
	port      int
	stopping  bool
	wg        sync.WaitGroup
}

// NewBroker returns a broker enforcing policy.
func NewBroker(policy EgressPolicy) *Broker { return &Broker{policy: policy} }

// SetOnDecision installs a sink called for every decision (the evidence seam).
func (b *Broker) SetOnDecision(f func(EgressDecision)) { b.onDecision = f }

// Start binds a loopback listener and serves in the background, returning the
// bound port.
func (b *Broker) Start() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	b.ln = ln
	b.port = ln.Addr().(*net.TCPAddr).Port
	b.wg.Add(1)
	go b.serve()
	return b.port, nil
}

// Port is the bound loopback port (0 before Start).
func (b *Broker) Port() int { return b.port }

// Stop closes the listener and waits for in-flight connections. Idempotent. A
// caller holding an idle tunnel should close it first.
func (b *Broker) Stop() {
	b.mu.Lock()
	if !b.stopping {
		b.stopping = true
		if b.ln != nil {
			_ = b.ln.Close()
		}
	}
	b.mu.Unlock()
	b.wg.Wait()
}

// Decisions returns a copy of the recorded decisions.
func (b *Broker) Decisions() []EgressDecision {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]EgressDecision(nil), b.decisions...)
}

func (b *Broker) record(d EgressDecision) {
	b.mu.Lock()
	b.decisions = append(b.decisions, d)
	f := b.onDecision
	b.mu.Unlock()
	if f != nil {
		f(d)
	}
}

func (b *Broker) serve() {
	defer b.wg.Done()
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.handle(conn)
		}()
	}
}

func (b *Broker) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	host, port, ok := readConnect(conn)
	if !ok {
		_, _ = conn.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
		return
	}

	decision, err := b.policy.Decide(host, port)
	if err != nil {
		// Fail closed: a lowering error denies, never allows silently.
		decision = EgressDecision{Host: host, Port: port, Verdict: VerdictDeny, Kind: "broker:connect-denied"}
	}
	b.record(decision)

	if decision.Verdict != VerdictAllow {
		_, _ = conn.Write([]byte("HTTP/1.1 403 Forbidden\r\n\r\n"))
		return
	}

	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 3*time.Second)
	if err != nil {
		_, _ = conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer upstream.Close()

	_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	_ = conn.SetDeadline(time.Time{}) // a tunnel is long-lived
	splice(conn, upstream)
}

// readConnect reads the request head and parses the CONNECT target.
func readConnect(conn net.Conn) (host string, port int, ok bool) {
	br := bufio.NewReader(io.LimitReader(conn, 8192))
	line, err := br.ReadString('\n')
	if err != nil {
		return "", 0, false
	}
	m := connectRe.FindStringSubmatch(line)
	if m == nil {
		return "", 0, false
	}
	p, err := strconv.Atoi(m[2])
	if err != nil {
		return "", 0, false
	}
	return m[1], p, true
}

// splice pumps bytes both ways until both directions close, propagating
// half-close so a client EOF tears the tunnel down.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(a, b); closeWrite(a) }()
	go func() { defer wg.Done(); _, _ = io.Copy(b, a); closeWrite(b) }()
	wg.Wait()
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}
