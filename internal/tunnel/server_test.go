package tunnel

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const (
	goodSlug  = "abcdefghijklmnopqrstuvwxyz"
	goodToken = "a-valid-token"
	goodID    = "endpoint-id-1"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testAuth accepts exactly one slug/token pair.
//
// The reason internal/tunnel can be tested with four lines instead of a
// database: AuthFunc is declared in this package, so nothing here depends on
// how endpoints are actually stored.
func testAuth(ctx context.Context, slug, token string) (string, error) {
	if slug == goodSlug && token == goodToken {
		return goodID, nil
	}
	return "", ErrUnauthorized
}

// newTestServer starts a tunnel on a real HTTP listener. Real, not
// httptest.NewRecorder, because a WebSocket upgrade needs a hijackable
// connection and a recorder cannot be hijacked.
func newTestServer(t *testing.T, opt Options) (*Server, *httptest.Server, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ts := &Server{
		log:              quiet(),
		auth:             testAuth,
		publicURL:        func(slug string) string { return "https://" + slug + ".example.test/" },
		baseCtx:          ctx,
		hub:              NewHub(),
		handshakeTimeout: firstNonZero(opt.HandshakeTimeout, defaultHandshakeTimeout),
		pingInterval:     firstNonZero(opt.PingInterval, defaultPingInterval),
		pongTimeout:      firstNonZero(opt.PongTimeout, defaultPongTimeout),
	}
	srv := httptest.NewServer(http.HandlerFunc(ts.Handle))
	t.Cleanup(srv.Close)
	t.Cleanup(cancel)
	return ts, srv, cancel
}

func dial(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func writeFrame(t *testing.T, c *websocket.Conn, typ Type, payload any) {
	t.Helper()
	b, err := Encode(typ, payload)
	if err != nil {
		t.Fatalf("encode %s: %v", typ, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write %s: %v", typ, err)
	}
}

func readFrame(t *testing.T, c *websocket.Conn, within time.Duration) Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	env, err := DecodeEnvelope(data)
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return env
}

// expectClose reads one frame and asserts it is a close with the given code.
func expectClose(t *testing.T, c *websocket.Conn, code string) Close {
	t.Helper()
	env := readFrame(t, c, 5*time.Second)
	if env.Type != TypeClose {
		t.Fatalf("got frame type %q, want %q", env.Type, TypeClose)
	}
	var cl Close
	if err := DecodePayload(env, &cl); err != nil {
		t.Fatalf("decode close: %v", err)
	}
	if cl.Code != code {
		t.Fatalf("close code = %q, want %q (reason: %q)", cl.Code, code, cl.Reason)
	}
	if cl.Reason == "" {
		t.Error("close frame has an empty reason; the CLI has nothing to print")
	}
	return cl
}

func TestHandshakeSucceeds(t *testing.T) {
	_, srv, _ := newTestServer(t, Options{})
	c := dial(t, srv)

	writeFrame(t, c, TypeHello, Hello{Slug: goodSlug, Token: goodToken, Version: ProtocolVersion})

	env := readFrame(t, c, 5*time.Second)
	if env.Type != TypeHelloOK {
		t.Fatalf("got %q, want %q", env.Type, TypeHelloOK)
	}
	var ok HelloOK
	if err := DecodePayload(env, &ok); err != nil {
		t.Fatalf("decode hello_ok: %v", err)
	}
	if ok.Slug != goodSlug {
		t.Errorf("slug = %q, want %q", ok.Slug, goodSlug)
	}
	if want := "https://" + goodSlug + ".example.test/"; ok.PublicURL != want {
		t.Errorf("public_url = %q, want %q", ok.PublicURL, want)
	}
	// The server dictates the ping interval so an old CLI cannot pin it.
	if ok.PingSeconds != int(defaultPingInterval/time.Second) {
		t.Errorf("ping_seconds = %d, want %d", ok.PingSeconds, int(defaultPingInterval/time.Second))
	}
}

func TestHandshakeRejects(t *testing.T) {
	// One table for every way the handshake can fail, because the thing worth
	// asserting is the same in each case: a close frame, the right code, and a
	// reason a human can read.
	tests := []struct {
		name string
		// send is what the client writes instead of a valid hello. A nil send
		// means "write nothing", which is the handshake-timeout case.
		send func(t *testing.T, c *websocket.Conn)
		code string
	}{
		{
			name: "bad token",
			send: func(t *testing.T, c *websocket.Conn) {
				writeFrame(t, c, TypeHello, Hello{Slug: goodSlug, Token: "wrong", Version: ProtocolVersion})
			},
			code: CodeUnauthorized,
		},
		{
			name: "unknown slug",
			send: func(t *testing.T, c *websocket.Conn) {
				writeFrame(t, c, TypeHello, Hello{Slug: "nosuchinbox", Token: goodToken, Version: ProtocolVersion})
			},
			code: CodeUnauthorized,
		},
		{
			name: "old client",
			send: func(t *testing.T, c *websocket.Conn) {
				writeFrame(t, c, TypeHello, Hello{Slug: goodSlug, Token: goodToken, Version: ProtocolVersion - 1})
			},
			code: CodeVersion,
		},
		{
			name: "first frame is not hello",
			send: func(t *testing.T, c *websocket.Conn) {
				writeFrame(t, c, TypeResponse, map[string]string{"req_id": "x"})
			},
			code: CodeMalformed,
		},
		{
			name: "not JSON",
			send: func(t *testing.T, c *websocket.Conn) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				if err := c.Write(ctx, websocket.MessageText, []byte("{not json")); err != nil {
					t.Fatalf("write: %v", err)
				}
			},
			code: CodeMalformed,
		},
		{
			name: "no type field",
			send: func(t *testing.T, c *websocket.Conn) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				if err := c.Write(ctx, websocket.MessageText, []byte(`{"payload":{}}`)); err != nil {
					t.Fatalf("write: %v", err)
				}
			},
			code: CodeMalformed,
		},
		{
			name: "binary frame",
			send: func(t *testing.T, c *websocket.Conn) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				if err := c.Write(ctx, websocket.MessageBinary, []byte{0x00, 0x01}); err != nil {
					t.Fatalf("write: %v", err)
				}
			},
			code: CodeMalformed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, srv, _ := newTestServer(t, Options{})
			c := dial(t, srv)
			tc.send(t, c)
			expectClose(t, c, tc.code)
		})
	}
}

// TestHandshakeTimeout is the security bound: the upgrade completes before
// authentication, so a connection that never authenticates must not be free.
func TestHandshakeTimeout(t *testing.T) {
	_, srv, _ := newTestServer(t, Options{HandshakeTimeout: 150 * time.Millisecond})
	c := dial(t, srv)

	// Say nothing at all.
	start := time.Now()
	cl := expectClose(t, c, CodeHandshake)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Errorf("took %v to close an unauthenticated connection", elapsed)
	}
	if !strings.Contains(cl.Reason, "hello") {
		t.Errorf("reason %q does not mention the missing hello", cl.Reason)
	}
}

// TestHandshakeDeadlineDoesNotLeakIntoConnection guards the specific bug the
// comment in handshake() warns about: if the 10s handshake context were reused
// for the serve loop, every healthy tunnel would die after ten seconds.
func TestHandshakeDeadlineDoesNotLeakIntoConnection(t *testing.T) {
	_, srv, _ := newTestServer(t, Options{
		HandshakeTimeout: 100 * time.Millisecond,
		PingInterval:     20 * time.Millisecond,
		PongTimeout:      time.Second,
	})
	c := dial(t, srv)
	writeFrame(t, c, TypeHello, Hello{Slug: goodSlug, Token: goodToken, Version: ProtocolVersion})
	if env := readFrame(t, c, 5*time.Second); env.Type != TypeHelloOK {
		t.Fatalf("got %q, want hello_ok", env.Type)
	}

	// Well past the handshake deadline, and past several ping cycles. The
	// connection must still be alive -- proven by a read that times out
	// waiting for data rather than failing because the socket closed.
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	_, _, err := c.Read(ctx)
	if err == nil {
		t.Fatal("unexpected frame; expected the connection to be idle but open")
	}
	if ctx.Err() == nil {
		t.Fatalf("connection died after the handshake deadline: %v", err)
	}
}

// TestPingKeepsConnectionAlive covers failure mode 7. Ping waits for the pong,
// so a connection surviving many ping cycles proves the round trip works.
func TestPingKeepsConnectionAlive(t *testing.T) {
	_, srv, _ := newTestServer(t, Options{
		PingInterval: 20 * time.Millisecond,
		PongTimeout:  time.Second,
	})
	c := dial(t, srv)
	writeFrame(t, c, TypeHello, Hello{Slug: goodSlug, Token: goodToken, Version: ProtocolVersion})
	readFrame(t, c, 5*time.Second)

	// The client library answers pings automatically while a read is pending.
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_, _, err := c.Read(ctx)
	if ctx.Err() == nil {
		t.Fatalf("connection dropped across ~15 ping cycles: %v", err)
	}
}

// TestShutdownClosesTunnels is why the tunnel holds a process-lifetime context
// instead of the request's: http.Server.Shutdown does not wait for, or even
// know about, hijacked connections.
func TestShutdownClosesTunnels(t *testing.T) {
	_, srv, cancel := newTestServer(t, Options{})
	c := dial(t, srv)
	writeFrame(t, c, TypeHello, Hello{Slug: goodSlug, Token: goodToken, Version: ProtocolVersion})
	if env := readFrame(t, c, 5*time.Second); env.Type != TypeHelloOK {
		t.Fatalf("got %q, want hello_ok", env.Type)
	}

	cancel() // the process is stopping

	cl := expectClose(t, c, CodeServerShutdown)
	if !strings.Contains(strings.ToLower(cl.Reason), "shutting down") {
		t.Errorf("reason %q does not say the server is shutting down", cl.Reason)
	}
}

// TestNoGoroutineLeak is the standing Phase 3 concern: every connection starts
// a read goroutine, and every exit path has to end it.
func TestNoGoroutineLeak(t *testing.T) {
	_, srv, _ := newTestServer(t, Options{
		HandshakeTimeout: 100 * time.Millisecond,
		PingInterval:     20 * time.Millisecond,
		PongTimeout:      500 * time.Millisecond,
	})

	settle(t)
	before := runtime.NumGoroutine()

	for range 20 {
		func() {
			c := dial(t, srv)
			writeFrame(t, c, TypeHello, Hello{Slug: goodSlug, Token: goodToken, Version: ProtocolVersion})
			readFrame(t, c, 5*time.Second)
			// Vanish without a closing handshake -- the realistic case. A CLI
			// whose laptop sleeps or loses Wi-Fi does not say goodbye.
			c.CloseNow()
		}()
	}

	settle(t)
	after := runtime.NumGoroutine()

	// A small allowance: the HTTP server keeps idle connection goroutines of
	// its own, and their teardown is not synchronised with ours.
	if after > before+5 {
		t.Errorf("goroutines grew from %d to %d across 20 connections", before, after)
	}
}

// settle waits for goroutine count to stop changing, so the leak assertion is
// not a race against teardown.
func settle(t *testing.T) {
	t.Helper()
	prev := -1
	for range 50 {
		time.Sleep(20 * time.Millisecond)
		n := runtime.NumGoroutine()
		if n == prev {
			return
		}
		prev = n
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	b, err := Encode(TypeHello, Hello{Slug: goodSlug, Token: goodToken, Version: 1})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	env, err := DecodeEnvelope(b)
	if err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Type != TypeHello {
		t.Fatalf("type = %q, want %q", env.Type, TypeHello)
	}

	var got Hello
	if err := DecodePayload(env, &got); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got.Slug != goodSlug || got.Token != goodToken || got.Version != 1 {
		t.Errorf("round trip lost data: %+v", got)
	}
}

// TestEnvelopeRejectsMissingType pins the one field every frame must carry.
// Without this check a `{}` frame decodes cleanly into a zero Envelope and is
// then compared against every known type, matching none, which produces a
// confusing "unexpected frame type" instead of "missing type".
func TestEnvelopeRejectsMissingType(t *testing.T) {
	for _, in := range []string{`{}`, `{"payload":{"a":1}}`} {
		if _, err := DecodeEnvelope([]byte(in)); err == nil {
			t.Errorf("DecodeEnvelope(%s) = nil error, want one", in)
		}
	}
}

// TestDecodePayloadRejectsEmpty stops a frame that names a type but carries
// nothing from decoding into a zero-valued struct. A hello with no payload
// would otherwise arrive as an empty slug and token and be reported as an
// authentication failure, sending its owner to look at the wrong thing.
func TestDecodePayloadRejectsEmpty(t *testing.T) {
	var h Hello
	if err := DecodePayload(Envelope{Type: TypeHello}, &h); err == nil {
		t.Error("expected an error for an empty payload")
	}
}

func TestEncodeProducesOneJSONObject(t *testing.T) {
	// The plan said newline-delimited JSON; WebSocket already frames messages,
	// so each frame must be exactly one object with no trailing newline.
	b, err := Encode(TypeClose, Close{Code: CodeUnauthorized, Reason: "nope"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(b), "\n") {
		t.Errorf("frame contains a newline: %q", b)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("frame is not a single JSON object: %v", err)
	}
}
