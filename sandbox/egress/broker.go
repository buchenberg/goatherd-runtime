package egress

import (
	"bytes"
	"errors"
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
// point the paddock's network pin at it, Stop it on scope close.
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
func (b *Broker) SetOnDecision(f func(EgressDecision)) {
	b.mu.Lock()
	b.onDecision = f
	b.mu.Unlock()
}

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

func (b *Broker) isStopping() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stopping
}

func (b *Broker) serve() {
	defer b.wg.Done()
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			// Only a closed listener ends the loop; a transient Accept error
			// (EMFILE/ECONNABORTED) must not silently stop the broker while
			// Port() still reports it up.
			if errors.Is(err, net.ErrClosed) || b.isStopping() {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
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

// readConnect reads the CONNECT request head — through the terminating blank line
// (`\r\n\r\n`, or a bare `\n\n`) — one byte at a time, so bytes the client
// pipelines after the head stay in the socket for the spliced tunnel (a
// bufio.Reader would swallow them into its buffer). It parses only the request
// line.
func readConnect(conn net.Conn) (host string, port int, ok bool) {
	buf := make([]byte, 0, 256)
	one := make([]byte, 1)
	for len(buf) < 8192 {
		n, err := conn.Read(one)
		if n > 0 {
			buf = append(buf, one[0])
			// Tolerate both a CRLF and an LF head terminator, so an LF-only
			// client does not push the loop past the blank line into the
			// tunnel bytes.
			if bytes.HasSuffix(buf, []byte("\r\n\r\n")) || bytes.HasSuffix(buf, []byte("\n\n")) {
				break
			}
		}
		if err != nil {
			break
		}
	}
	line := buf
	if i := bytes.IndexByte(buf, '\n'); i >= 0 {
		line = buf[:i]
	}
	m := connectRe.FindStringSubmatch(string(line))
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
