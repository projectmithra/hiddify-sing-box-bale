// Package v2raybale implements a SingBox V2RayClientTransport that wraps
// tunnel traffic inside Bale messenger's protobuf wire format for DPI-resistant
// censorship circumvention.
//
// This file is the SingBox-adapter wrapper; the pure protobuf codec lives in
// the upstream projectmithra/bale-transport module (package bale).
package v2raybale

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/projectmithra/bale-transport/bale"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/tls"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	sHTTP "github.com/sagernet/sing/protocol/http"
	"github.com/sagernet/ws"
	"github.com/sagernet/ws/wsutil"
)

var _ adapter.V2RayClientTransport = (*Client)(nil)

// Process-local PRNG for jitter and service-selection randomness. Seeded once
// at package init from crypto/rand so the sequence is non-deterministic across
// runs even though the per-call randomness is not cryptographic.
var (
	jitterRand   *rand.Rand
	jitterRandMu sync.Mutex
)

func init() {
	var seed [8]byte
	if _, err := cryptorand.Read(seed[:]); err != nil {
		// crypto/rand failure is effectively impossible on supported
		// platforms; fall back to a time-based seed so the package can
		// still load. This path is not reachable in practice.
		jitterRand = rand.New(rand.NewSource(time.Now().UnixNano()))
		return
	}
	jitterRand = rand.New(rand.NewSource(int64(binary.LittleEndian.Uint64(seed[:]))))
}

// randInt63n is jitterRand.Int63n made goroutine-safe.
func randInt63n(n int64) int64 {
	jitterRandMu.Lock()
	defer jitterRandMu.Unlock()
	return jitterRand.Int63n(n)
}

// randIntn is jitterRand.Intn made goroutine-safe.
func randIntn(n int) int {
	jitterRandMu.Lock()
	defer jitterRandMu.Unlock()
	return jitterRand.Intn(n)
}

// randInt31 returns a goroutine-safe random int31 used to seed per-connection
// request counters.
func randInt31() int32 {
	jitterRandMu.Lock()
	defer jitterRandMu.Unlock()
	return jitterRand.Int31()
}

// ----------------------------------------------------------------------------
// SERVICE / METHOD SELECTION
// ----------------------------------------------------------------------------
//
// Real Bale web-client traffic is dominated by a handful of service/method
// pairs; other pairs appear only rarely. A deterministic modulo rotation
// across the service and method lists produces a uniform distribution and a
// repeating period — both visible to a traffic-analysis observer.
//
// weightedServiceMethods lists the realistic (service, method) pairs with
// integer weights roughly proportional to their observed frequency in real
// Bale web-client traffic. Values are approximate; tune against a larger
// capture if you have one. Total = 100 for easy readability.

type svcMethod struct {
	service string
	method  string
	weight  int
}

var weightedServiceMethods = []svcMethod{
	{"bale.v1.Configs", "GetParameters", 45},
	{"bale.users.v1.Users", "GetContacts", 15},
	{"bale.users.v1.Users", "LoadUsers", 10},
	{"bale.users.v1.Users", "SearchContacts", 5},
	{"bale.fanoos.v1.fanoos", "Send", 8},
	{"bale.auth.v1.Auth", "ImportContacts", 5},
	{"bale.ramz.v1.Ramz", "GetContacts", 5},
	{"bale.feedback.v1.FeedBack", "Send", 3},
	{"bale.report.v1.Report", "Send", 2},
	{"ai.bale.pushak.Push", "Send", 2},
}

var svcMethodTotalWeight int

func init() {
	for _, p := range weightedServiceMethods {
		svcMethodTotalWeight += p.weight
	}
}

// pickServiceMethod returns a weighted-random (service, method) pair that
// matches the distribution of real Bale web-client traffic.
func pickServiceMethod() (string, string) {
	r := randIntn(svcMethodTotalWeight)
	for _, p := range weightedServiceMethods {
		if r < p.weight {
			return p.service, p.method
		}
		r -= p.weight
	}
	// unreachable unless weighting drifts; return the highest-frequency pair.
	return weightedServiceMethods[0].service, weightedServiceMethods[0].method
}

// wrapTunnelDataWeighted mirrors bale.WrapTunnelData but substitutes a
// weighted-random (service, method) pair in place of the round-robin
// selection built into the upstream helper. This avoids the modulo cycle
// that would otherwise be visible to a passive traffic-analysis observer.
func wrapTunnelDataWeighted(tunnelBytes []byte, requestIndex int) []byte {
	if len(tunnelBytes) > bale.MaxPayloadSize {
		tunnelBytes = tunnelBytes[:bale.MaxPayloadSize]
	}
	service, method := pickServiceMethod()
	padded := bale.AddPadding(tunnelBytes)
	req := bale.NewPbWriter()
	req.WriteString(1, service)
	req.WriteString(2, method)
	req.WriteBytes(3, padded)
	req.WriteTag(5, 0)
	req.WriteVarint(uint32(requestIndex))
	env := bale.NewPbWriter()
	env.WriteBytes(1, req.Finish())
	return env.Finish()
}

// ----------------------------------------------------------------------------
// CLIENT
// ----------------------------------------------------------------------------

// Client is the SingBox V2RayClientTransport implementation for Bale protocol
// mimicry. It tracks active connections so Close can cascade.
type Client struct {
	dialer     N.Dialer
	serverAddr M.Socksaddr
	requestURL url.URL
	headers    http.Header
	options    option.V2RayBaleOptions

	mu     sync.Mutex
	conns  map[*baleConn]struct{}
	closed bool
}

// NewClient returns a client configured to dial a Bale-camouflaged WebSocket
// endpoint. When options.WorkerHost is set the caller is responsible for
// supplying an Origin and AcceptLanguage that match the WorkerHost — the
// Bale-specific defaults are only applied when WorkerHost is empty, because
// using "https://web.bale.ai" as Origin against a non-Bale Worker host is a
// camouflage leak.
func NewClient(ctx context.Context, dialer N.Dialer, serverAddr M.Socksaddr, options option.V2RayBaleOptions, tlsConfig tls.Config) (adapter.V2RayClientTransport, error) {
	_ = ctx // retained for signature compatibility; used by dialer downstream

	if tlsConfig != nil {
		if len(tlsConfig.NextProtos()) == 0 {
			tlsConfig.SetNextProtos([]string{"http/1.1"})
		}
		dialer = tls.NewDialer(dialer, tlsConfig)
	}

	var requestURL url.URL
	if tlsConfig == nil {
		requestURL.Scheme = "ws"
	} else {
		requestURL.Scheme = "wss"
	}
	requestURL.Host = serverAddr.String()

	path := options.Path
	if path == "" {
		path = "/w"
	}
	if err := sHTTP.URLSetPath(&requestURL, path); err != nil {
		return nil, E.Cause(err, "parse path")
	}
	if !strings.HasPrefix(requestURL.Path, "/") {
		requestURL.Path = "/" + requestURL.Path
	}

	headers := options.Headers.Build()

	usingWorkerHost := options.WorkerHost != ""

	// Origin: Bale default only when we are actually talking to Bale.
	origin := options.Origin
	if origin == "" {
		if usingWorkerHost {
			return nil, E.New("bale transport: origin is required when worker_host is set")
		}
		origin = "https://web.bale.ai"
	}
	headers.Set("Origin", origin)

	acceptLanguage := options.AcceptLanguage
	if acceptLanguage == "" {
		if usingWorkerHost {
			// Sensible multi-lingual default that does not pin the
			// client to Iranian locale when fronting arbitrary hosts.
			acceptLanguage = "en-US,en;q=0.9"
		} else {
			acceptLanguage = "fa-IR,fa;q=0.9,en-US;q=0.8,en;q=0.7"
		}
	}
	headers.Set("Accept-Language", acceptLanguage)

	// Protocol headers are Bale-specific; allow override so the same
	// transport can be retargeted at Bale protocol look-alikes without
	// editing source.
	baleProto := options.BaleProto
	if baleProto == "" {
		baleProto = "1"
	}
	headers.Set("X-Bale-Proto", baleProto)

	wsSubproto := options.WebSocketSubprotocol
	if wsSubproto == "" {
		wsSubproto = "binary"
	}
	// headers.Set("Sec-WebSocket-Protocol", wsSubproto)

	if headers.Get("User-Agent") == "" {
		headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	}

	if host := headers.Get("Host"); host != "" {
		headers.Del("Host")
		requestURL.Host = host
	}
	if usingWorkerHost {
		requestURL.Host = options.WorkerHost
	}

	return &Client{
		dialer:     dialer,
		serverAddr: serverAddr,
		requestURL: requestURL,
		headers:    headers,
		options:    options,
		conns:      make(map[*baleConn]struct{}),
	}, nil
}

// DialContext opens a TCP connection, upgrades to WebSocket, performs the
// Bale protobuf handshake, and returns a net.Conn that transparently wraps
// and unwraps all subsequent payloads in Bale envelopes.
func (c *Client) DialContext(ctx context.Context) (net.Conn, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, net.ErrClosed
	}
	c.mu.Unlock()

	conn, err := c.dialer.DialContext(ctx, N.NetworkTCP, c.serverAddr)
	if err != nil {
		return nil, err
	}

	d := ws.Dialer{Header: ws.HandshakeHeaderHTTP(c.headers)}
	if _, _, err := d.Upgrade(conn, &c.requestURL); err != nil {
		conn.Close()
		return nil, E.Cause(err, "bale ws upgrade")
	}

	wc := &baleWs{
		conn:  conn,
		state: ws.StateClientSide,
		reader: &wsutil.Reader{
			Source:         conn,
			State:          ws.StateClientSide,
			OnIntermediate: wsutil.ControlFrameHandler(conn, ws.StateClientSide),
		},
		ctrlH: wsutil.ControlFrameHandler(conn, ws.StateClientSide),
	}

	if err := baleHandshake(wc); err != nil {
		conn.Close()
		return nil, E.Cause(err, "bale handshake")
	}

	bc := newBaleConn(wc, c.serverAddr, c.removeConn)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		bc.Close()
		return nil, net.ErrClosed
	}
	c.conns[bc] = struct{}{}
	c.mu.Unlock()

	return bc, nil
}

// Close closes every live connection produced by this Client.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	conns := make([]*baleConn, 0, len(c.conns))
	for bc := range c.conns {
		conns = append(conns, bc)
	}
	c.conns = nil
	c.mu.Unlock()

	var firstErr error
	for _, bc := range conns {
		if err := bc.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (c *Client) removeConn(bc *baleConn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conns == nil {
		return
	}
	delete(c.conns, bc)
}

// ----------------------------------------------------------------------------
// WEBSOCKET FRAME LAYER
// ----------------------------------------------------------------------------

// maxBaleFrame caps the size of a single inbound WebSocket binary frame.
// Real Bale frames never approach this; the cap exists purely to make a
// hostile or misconfigured server unable to force an arbitrarily large
// allocation on the client.
const maxBaleFrame = 1 << 22 // 4 MiB — matches bale.MaxPayloadSize + envelope

type baleWs struct {
	conn  net.Conn
	state ws.State
	// reader and ctrlH are only touched by the single goroutine inside
	// baleConn.Read (and by baleHandshake before that goroutine exists).
	// Do not call read() from multiple goroutines.
	reader *wsutil.Reader
	ctrlH  wsutil.FrameHandlerFunc
	wmu    sync.Mutex // protects write(); read path has no equivalent.
}

func (w *baleWs) write(data []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	return wsutil.WriteMessage(w.conn, w.state, ws.OpBinary, data)
}

func (w *baleWs) read() ([]byte, error) {
	for {
		hdr, err := w.reader.NextFrame()
		if err != nil {
			return nil, err
		}
		if hdr.OpCode.IsControl() {
			if err := w.ctrlH(hdr, w.reader); err != nil {
				return nil, err
			}
			continue
		}
		if hdr.OpCode&ws.OpBinary == 0 {
			if err := w.reader.Discard(); err != nil {
				return nil, err
			}
			continue
		}
		if hdr.Length < 0 || hdr.Length > maxBaleFrame {
			return nil, E.New("bale: frame length out of range")
		}
		buf := make([]byte, hdr.Length)
		if _, err := io.ReadFull(w.reader, buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
}

// maxHandshakeFrames caps the number of non-handshake frames accepted before
// the handshake phase is declared failed, preventing a misbehaving peer from
// holding a handshake-phase goroutine open indefinitely within the 10-second
// read deadline.
const maxHandshakeFrames = 8

func baleHandshake(w *baleWs) error {
	if err := w.write(bale.EncodeHandshakeRequest()); err != nil {
		return err
	}
	if err := w.conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	defer w.conn.SetReadDeadline(time.Time{})

	for i := 0; i < maxHandshakeFrames; i++ {
		data, err := w.read()
		if err != nil {
			return err
		}
		env, err := bale.DecodeServerEnvelope(data)
		if err != nil {
			return err
		}
		if env.Type == "handshake" {
			return nil
		}
	}
	return E.New("bale: no handshake envelope received within ", maxHandshakeFrames, " frames")
}

// ----------------------------------------------------------------------------
// BALECONN — net.Conn over protobuf-wrapped WebSocket
// ----------------------------------------------------------------------------

type baleConn struct {
	w      *baleWs
	rbuf   []byte
	ridx   int32
	pid    int32
	closed int32
	done   chan struct{}
	sa     M.Socksaddr

	onClose func(*baleConn)
}

func newBaleConn(w *baleWs, sa M.Socksaddr, onClose func(*baleConn)) *baleConn {
	c := &baleConn{
		w:       w,
		done:    make(chan struct{}),
		sa:      sa,
		ridx:    randInt31(),
		onClose: onClose,
	}
	go c.pinger()
	return c
}

func (c *baleConn) Read(b []byte) (int, error) {
	if len(c.rbuf) > 0 {
		n := copy(b, c.rbuf)
		c.rbuf = c.rbuf[n:]
		return n, nil
	}

	for {
		if atomic.LoadInt32(&c.closed) != 0 {
			return 0, io.EOF
		}
		data, err := c.w.read()
		if err != nil {
			return 0, err
		}
		env, err := bale.DecodeServerEnvelope(data)
		if err != nil {
			// Malformed envelope from upstream. Skip silently rather
			// than tearing down the connection — Bale occasionally
			// emits frames the codec does not recognise.
			continue
		}

		switch env.Type {
		case "response", "update":
			payload, err := bale.ExtractPayload(env)
			if err != nil || payload == nil {
				continue
			}
			clean := bale.StripPadding(payload)
			if len(clean) == 0 {
				continue
			}
			n := copy(b, clean)
			if n < len(clean) {
				c.rbuf = clean[n:]
			}
			return n, nil
		case "terminate":
			return 0, io.EOF
		}
	}
}

func (c *baleConn) Write(b []byte) (int, error) {
	if atomic.LoadInt32(&c.closed) != 0 {
		return 0, io.ErrClosedPipe
	}
	idx := int(atomic.AddInt32(&c.ridx, 1))
	if err := c.w.write(wrapTunnelDataWeighted(b, idx)); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Close tears down the connection. It is safe to call from any goroutine and
// safe to call multiple times. Setting the read deadline to a past time
// unblocks any in-flight Read() so callers see io.EOF promptly instead of
// hanging on a half-closed socket.
func (c *baleConn) Close() error {
	if !atomic.CompareAndSwapInt32(&c.closed, 0, 1) {
		return nil
	}
	close(c.done)
	// Wake any blocked Read by poisoning the read deadline before closing.
	_ = c.w.conn.SetReadDeadline(time.Unix(1, 0))
	err := c.w.conn.Close()
	if c.onClose != nil {
		c.onClose(c)
	}
	return err
}

func (c *baleConn) LocalAddr() net.Addr  { return c.w.conn.LocalAddr() }
func (c *baleConn) RemoteAddr() net.Addr { return c.w.conn.RemoteAddr() }

// Deadlines are forwarded to the underlying net.Conn. Note that the deadline
// applies to raw socket I/O: a Read() that returns a single tunnel byte may
// have internally consumed multiple WebSocket frames, each subject to the
// deadline individually.
func (c *baleConn) SetDeadline(t time.Time) error      { return c.w.conn.SetDeadline(t) }
func (c *baleConn) SetReadDeadline(t time.Time) error  { return c.w.conn.SetReadDeadline(t) }
func (c *baleConn) SetWriteDeadline(t time.Time) error { return c.w.conn.SetWriteDeadline(t) }

func (c *baleConn) NeedAdditionalReadDeadline() bool { return true }
func (c *baleConn) Upstream() any                    { return c.w.conn }

// pinger mimics Bale's production web client: one warm-up ping within the
// first 20–30 seconds, then pings at 25s ±3s. Uses the package-level PRNG
// for jitter; a write failure means the upstream connection is gone and the
// goroutine exits so the connection can be reaped.
func (c *baleConn) pinger() {
	initial := 20*time.Second + time.Duration(randInt63n(int64(10*time.Second)))
	select {
	case <-time.After(initial):
	case <-c.done:
		return
	}
	for atomic.LoadInt32(&c.closed) == 0 {
		id := int(atomic.AddInt32(&c.pid, 1))
		if err := c.w.write(bale.EncodePing(id)); err != nil {
			// Upstream is gone — trigger a full Close so the
			// read goroutine wakes and any client observing this
			// net.Conn sees an EOF/closed state.
			_ = c.Close()
			return
		}
		jitter := time.Duration(randInt63n(6000))*time.Millisecond - 3*time.Second
		select {
		case <-time.After(25*time.Second + jitter):
		case <-c.done:
			return
		}
	}
}
