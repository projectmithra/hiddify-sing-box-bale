package v2raybale

import (
"context"
"encoding/binary"
"io"
"math/rand"
"net"
"net/http"
"net/url"
"os"
"strings"
"sync"
"sync/atomic"
"time"

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

type Client struct {
dialer     N.Dialer
serverAddr M.Socksaddr
requestURL url.URL
headers    http.Header
options    option.V2RayBaleOptions
}

func NewClient(ctx context.Context, dialer N.Dialer, serverAddr M.Socksaddr, options option.V2RayBaleOptions, tlsConfig tls.Config) (adapter.V2RayClientTransport, error) {
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
err := sHTTP.URLSetPath(&requestURL, path)
if err != nil {
return nil, E.Cause(err, "parse path")
}
if !strings.HasPrefix(requestURL.Path, "/") {
requestURL.Path = "/" + requestURL.Path
}
headers := options.Headers.Build()
origin := options.Origin
if origin == "" {
origin = "https://web.bale.ai"
}
headers.Set("Origin", origin)
al := options.AcceptLanguage
if al == "" {
al = "fa-IR,fa;q=0.9,en-US;q=0.8,en;q=0.7"
}
headers.Set("Accept-Language", al)
headers.Set("X-Bale-Proto", "1")
if headers.Get("User-Agent") == "" {
headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
}
if host := headers.Get("Host"); host != "" {
headers.Del("Host")
requestURL.Host = host
}
if options.WorkerHost != "" {
requestURL.Host = options.WorkerHost
}
return &Client{dialer: dialer, serverAddr: serverAddr, requestURL: requestURL, headers: headers, options: options}, nil
}

func (c *Client) DialContext(ctx context.Context) (net.Conn, error) {
conn, err := c.dialer.DialContext(ctx, N.NetworkTCP, c.serverAddr)
if err != nil {
return nil, err
}
d := ws.Dialer{Header: ws.HandshakeHeaderHTTP(c.headers)}
_, _, err = d.Upgrade(conn, &c.requestURL)
	rawConn := conn
if err != nil {
conn.Close()
return nil, E.Cause(err, "bale ws upgrade")
}
wc := &baleWs{conn: rawConn, state: ws.StateClientSide,
reader: &wsutil.Reader{Source: rawConn, State: ws.StateClientSide,
OnIntermediate: wsutil.ControlFrameHandler(rawConn, ws.StateClientSide)},
ctrlH: wsutil.ControlFrameHandler(rawConn, ws.StateClientSide)}
if err := baleHandshake(wc); err != nil {
rawConn.Close()
return nil, E.Cause(err, "bale handshake")
}
return newBaleConn(wc, c.serverAddr), nil
}

func (c *Client) Close() error { return nil }

// --- low-level WS ---
type baleWs struct {
conn  net.Conn
state ws.State
reader *wsutil.Reader
ctrlH  wsutil.FrameHandlerFunc
wmu   sync.Mutex
}

func (w *baleWs) write(data []byte) error {
w.wmu.Lock()
defer w.wmu.Unlock()
return wsutil.WriteMessage(w.conn, w.state, ws.OpBinary, data)
}

func (w *baleWs) read() ([]byte, error) {
for {
hdr, err := w.reader.NextFrame()
if err != nil { return nil, err }
if hdr.OpCode.IsControl() {
if err := w.ctrlH(hdr, w.reader); err != nil { return nil, err }
continue
}
if hdr.OpCode&ws.OpBinary == 0 {
w.reader.Discard()
continue
}
buf := make([]byte, hdr.Length)
_, err = io.ReadFull(w.reader, buf)
return buf, err
}
}

func baleHandshake(w *baleWs) error {
if err := w.write(encHS()); err != nil { return err }
w.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
defer w.conn.SetReadDeadline(time.Time{})
for {
data, err := w.read()
if err != nil { return err }
env := decEnv(data)
if env.t == "handshake" { return nil }
}
}

// --- BaleConn: net.Conn over protobuf-wrapped WS ---
type baleConn struct {
w       *baleWs
rbuf    []byte
ridx    int32
pid     int32
closed  int32
done    chan struct{}
sa      M.Socksaddr
}

func newBaleConn(w *baleWs, sa M.Socksaddr) *baleConn {
c := &baleConn{w: w, done: make(chan struct{}), sa: sa}
go c.pinger()
return c
}

func (c *baleConn) Read(b []byte) (int, error) {
if len(c.rbuf) > 0 {
n := copy(b, c.rbuf); c.rbuf = c.rbuf[n:]; return n, nil
}
for {
if atomic.LoadInt32(&c.closed) != 0 { return 0, io.EOF }
data, err := c.w.read()
if err != nil { return 0, err }
env := decEnv(data)
if env.t == "response" || env.t == "update" {
p := extPay(env)
if p == nil { continue }
cl := stripPad(p)
if len(cl) == 0 { continue }
n := copy(b, cl)
if n < len(cl) { c.rbuf = cl[n:] }
return n, nil
}
if env.t == "terminate" { return 0, io.EOF }
}
}

func (c *baleConn) Write(b []byte) (int, error) {
if atomic.LoadInt32(&c.closed) != 0 { return 0, io.ErrClosedPipe }
idx := int(atomic.AddInt32(&c.ridx, 1))
if err := c.w.write(wrapTD(b, idx)); err != nil { return 0, err }
return len(b), nil
}

func (c *baleConn) Close() error {
if !atomic.CompareAndSwapInt32(&c.closed, 0, 1) { return nil }
close(c.done); return c.w.conn.Close()
}

func (c *baleConn) LocalAddr() net.Addr                { return c.w.conn.LocalAddr() }
func (c *baleConn) RemoteAddr() net.Addr               { return c.w.conn.RemoteAddr() }
func (c *baleConn) SetDeadline(t time.Time) error      { return os.ErrInvalid }
func (c *baleConn) SetReadDeadline(t time.Time) error   { return os.ErrInvalid }
func (c *baleConn) SetWriteDeadline(t time.Time) error  { return os.ErrInvalid }
func (c *baleConn) NeedAdditionalReadDeadline() bool     { return true }
func (c *baleConn) Upstream() any                        { return c.w.conn }

func (c *baleConn) pinger() {
d := 20*time.Second + time.Duration(rand.Int63n(int64(10*time.Second)))
select { case <-time.After(d): case <-c.done: return }
for atomic.LoadInt32(&c.closed) == 0 {
id := int(atomic.AddInt32(&c.pid, 1))
c.w.write(encPing(id))
j := time.Duration(rand.Int63n(6000))*time.Millisecond - 3*time.Second
select { case <-time.After(25*time.Second + j): case <-c.done: return }
}
}

// === PROTOBUF CODEC ===
var svcs = []string{"bale.v1.Configs","bale.users.v1.Users","bale.auth.v1.Auth","bale.fanoos.v1.fanoos","bale.feedback.v1.FeedBack","bale.ramz.v1.Ramz","bale.report.v1.Report","ai.bale.pushak.Push"}
var mths = []string{"GetParameters","GetContacts","LoadUsers","SearchContacts","ImportContacts","Send"}

type pb struct{ b []byte }
func npb() *pb { return &pb{b: make([]byte, 0, 256)} }
func (w *pb) vi(v uint32) { for v > 0x7F { w.b = append(w.b, byte(v&0x7F)|0x80); v >>= 7 }; w.b = append(w.b, byte(v&0x7F)) }
func (w *pb) tg(f, wt int) { w.vi(uint32((f << 3) | wt)) }
func (w *pb) by(f int, d []byte) { w.tg(f, 2); w.vi(uint32(len(d))); w.b = append(w.b, d...) }
func (w *pb) st(f int, s string) { w.by(f, []byte(s)) }
func (w *pb) i32(f, v int) { if v != 0 { w.tg(f, 0); w.vi(uint32(v)) } }

func encHS() []byte {
h := npb(); h.i32(1, 1); h.i32(2, 1)
e := npb(); e.by(3, h.b); return e.b
}
func encPing(id int) []byte {
p := npb(); if id != 0 { p.tg(1, 0); p.vi(uint32(id)) }
e := npb(); e.by(2, p.b); return e.b
}
func wrapTD(data []byte, idx int) []byte {
pd := addPad(data)
r := npb(); r.st(1, svcs[idx%len(svcs)]); r.st(2, mths[idx%len(mths)]); r.by(3, pd); r.tg(5, 0); r.vi(uint32(idx))
e := npb(); e.by(1, r.b); return e.b
}
func addPad(d []byte) []byte {
dl := len(d); ts := dl
switch { case dl<50: ts=50+rand.Intn(150); case dl<500: ts=mx(dl,200)+rand.Intn(300); case dl<4096: ts=dl+rand.Intn(512); default: ts=dl+rand.Intn(64) }
if ts<=dl { ts=dl }
r := make([]byte, 2+dl+(ts-dl))
binary.BigEndian.PutUint16(r[0:2], uint16(dl)); copy(r[2:], d)
for i := 2+dl; i < len(r); i++ { r[i]=byte(rand.Intn(256)) }; return r
}
func stripPad(d []byte) []byte {
if len(d)<2 { return d }; rl:=int(binary.BigEndian.Uint16(d[0:2]))
if rl+2>len(d) { return d }; return d[2:2+rl]
}
func mx(a, b int) int { if a>b { return a }; return b }

type env struct { t string; resp, upd []byte }
func decEnv(d []byte) *env {
e := &env{t: "unknown"}; pos := 0
for pos < len(d) {
tg := d[pos]; pos++; fn := int(tg>>3); wt := int(tg&0x07)
if wt == 2 {
ln, n := rv(d[pos:]); pos += n; end := pos+int(ln)
if end > len(d) { break }
switch fn { case 1: e.resp=d[pos:end]; e.t="response"; case 2: e.upd=d[pos:end]; e.t="update"; case 3: e.t="terminate"; case 4: e.t="pong"; case 5: e.t="handshake" }
pos = end
} else if wt == 0 { _, n := rv(d[pos:]); pos += n } else { break }
}; return e
}
func extPay(e *env) []byte {
d := e.resp; if d==nil { d=e.upd }; if d==nil { return nil }
pos := 0
for pos < len(d) {
tg := d[pos]; pos++; fn := int(tg>>3); wt := int(tg&0x07)
if wt == 2 { ln, n := rv(d[pos:]); pos += n; if fn==1||fn==2 { end:=pos+int(ln); if end<=len(d) { return d[pos:end] } }; pos+=int(ln)
} else if wt==0 { _, n := rv(d[pos:]); pos += n } else { break }
}; return nil
}
func rv(d []byte) (uint32, int) {
var r uint32; var s uint
for i := 0; i < len(d) && i < 5; i++ { b:=d[i]; r|=uint32(b&0x7F)<<s; if b&0x80==0 { return r, i+1 }; s+=7 }
return r, 1
}
