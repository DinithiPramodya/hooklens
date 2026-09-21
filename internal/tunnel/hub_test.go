package tunnel

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeCLI is the client half of the tunnel: it completes the handshake, then
// answers request frames with whatever `respond` returns.
//
// respond runs in its own goroutine per request, so a handler that sleeps
// does not stop the next request being read. That is deliberate -- a fake
// that handled requests serially could not demonstrate out-of-order
// responses, which is the whole point of this unit.
type fakeCLI struct {
	conn *websocket.Conn
	done chan struct{}
}

func startFakeCLI(t *testing.T, srv *httptest.Server, respond func(Request) *Response) *fakeCLI {
	t.Helper()
	c := dial(t, srv)
	writeFrame(t, c, TypeHello, Hello{Slug: goodSlug, Token: goodToken, Version: ProtocolVersion})
	if env := readFrame(t, c, 5*time.Second); env.Type != TypeHelloOK {
		t.Fatalf("handshake: got %q, want hello_ok", env.Type)
	}

	f := &fakeCLI{conn: c, done: make(chan struct{})}
	go func() {
		defer close(f.done)
		for {
			_, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			env, err := DecodeEnvelope(data)
			if err != nil || env.Type != TypeRequest {
				continue
			}
			var req Request
			if err := DecodePayload(env, &req); err != nil {
				continue
			}
			go func() {
				resp := respond(req)
				if resp == nil {
					return // deliberately never answer
				}
				resp.ReqID = req.ReqID
				b, err := Encode(TypeResponse, resp)
				if err != nil {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = c.Write(ctx, websocket.MessageText, b)
			}()
		}
	}()
	return f
}

// waitConnected blocks until the hub has registered a tunnel for goodID.
// The handshake completing on the client side does not mean the server has
// finished registering, and asserting on that race would make these tests
// flaky rather than wrong.
func waitConnected(t *testing.T, h *Hub) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.Connected(goodID) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("tunnel never registered with the hub")
}

func TestForwardRoundTrip(t *testing.T) {
	ts, srv, _ := newTestServer(t, Options{})
	startFakeCLI(t, srv, func(req Request) *Response {
		return &Response{
			Status:  201,
			Headers: []Header{{Name: "X-Echo", Value: req.Path}},
			BodyB64: "aGVsbG8=", // "hello"
		}
	})
	waitConnected(t, ts.Hub())

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	resp, err := ts.Hub().Forward(ctx, goodID, Request{Method: "POST", Path: "/webhook"})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != 201 {
		t.Errorf("status = %d, want 201", resp.Status)
	}
	if len(resp.Headers) != 1 || resp.Headers[0].Value != "/webhook" {
		t.Errorf("headers = %+v, want the echoed path", resp.Headers)
	}
	if resp.BodyB64 != "aGVsbG8=" {
		t.Errorf("body = %q", resp.BodyB64)
	}
}

// TestForwardCorrelatesOutOfOrder is the unit.
//
// Twenty concurrent requests answered in REVERSE order, each with a body that
// identifies its request. Every caller must get its own answer. Without the
// correlation map -- matching by arrival order instead -- every single one of
// these would receive the wrong response, and with symmetric-looking payloads
// nobody would notice for months.
func TestForwardCorrelatesOutOfOrder(t *testing.T) {
	const n = 20

	var mu sync.Mutex
	held := make([]chan struct{}, 0, n)
	release := make(chan struct{})

	ts, srv, _ := newTestServer(t, Options{})
	startFakeCLI(t, srv, func(req Request) *Response {
		// Park every request until all n have arrived, then answer in reverse.
		gate := make(chan struct{})
		mu.Lock()
		held = append(held, gate)
		ready := len(held) == n
		mu.Unlock()
		if ready {
			close(release)
		}
		<-release
		<-gate
		return &Response{Status: 200, BodyB64: req.Path}
	})
	waitConnected(t, ts.Hub())

	type result struct {
		want string
		resp *Response
		err  error
	}
	results := make(chan result, n)

	for i := range n {
		path := "req-" + strconv.Itoa(i)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			resp, err := ts.Hub().Forward(ctx, goodID, Request{Method: "POST", Path: path})
			results <- result{want: path, resp: resp, err: err}
		}()
	}

	// Wait for all n to be parked, then release them newest-first.
	<-release
	mu.Lock()
	gates := held
	mu.Unlock()
	for i := len(gates) - 1; i >= 0; i-- {
		close(gates[i])
		time.Sleep(time.Millisecond) // widen the window for a mismatch to occur
	}

	for range n {
		select {
		case r := <-results:
			if r.err != nil {
				t.Fatalf("Forward(%s): %v", r.want, r.err)
			}
			if r.resp.BodyB64 != r.want {
				t.Errorf("CROSSED WIRES: caller for %q received the response for %q",
					r.want, r.resp.BodyB64)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for responses")
		}
	}

	assertNoPending(t, ts)
}

func TestForwardNoTunnel(t *testing.T) {
	ts, _, _ := newTestServer(t, Options{})
	_, err := ts.Hub().Forward(t.Context(), goodID, Request{Method: "GET", Path: "/"})
	if !errors.Is(err, ErrNoTunnel) {
		t.Fatalf("err = %v, want ErrNoTunnel", err)
	}
}

// TestForwardTimeout is failure mode 3: the local app hangs. Ingest must
// unblock, and nothing may be left in the correlation map.
func TestForwardTimeout(t *testing.T) {
	ts, srv, _ := newTestServer(t, Options{})
	startFakeCLI(t, srv, func(Request) *Response { return nil }) // never answers
	waitConnected(t, ts.Hub())

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := ts.Hub().Forward(ctx, goodID, Request{Method: "GET", Path: "/slow"})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %v to give up on a 200ms deadline", elapsed)
	}
	assertNoPending(t, ts)
}

// TestForwardDisconnectUnblocksImmediately is failure mode 4, and the word
// that matters is "immediately". The deadline here is 30 seconds; if the
// implementation merely let callers time out, this test would take 30s
// instead of milliseconds.
func TestForwardDisconnectUnblocksImmediately(t *testing.T) {
	ts, srv, _ := newTestServer(t, Options{})
	cli := startFakeCLI(t, srv, func(Request) *Response { return nil })
	waitConnected(t, ts.Hub())

	const inFlight = 5
	errs := make(chan error, inFlight)
	for range inFlight {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, err := ts.Hub().Forward(ctx, goodID, Request{Method: "GET", Path: "/hang"})
			errs <- err
		}()
	}

	// Give the requests time to reach the map before pulling the plug.
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	cli.conn.CloseNow()

	for range inFlight {
		select {
		case err := <-errs:
			if !errors.Is(err, ErrDisconnected) {
				t.Errorf("err = %v, want ErrDisconnected", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a caller was still blocked 5s after the tunnel dropped; " +
				"it is waiting out its own deadline instead of being woken")
		}
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %v to unblock callers after a drop", elapsed)
	}
}

// TestSecondClientWins is failure mode 5.
func TestSecondClientWins(t *testing.T) {
	ts, srv, _ := newTestServer(t, Options{})

	// First client, connected by hand so its close frame can be read.
	first := dial(t, srv)
	writeFrame(t, first, TypeHello, Hello{Slug: goodSlug, Token: goodToken, Version: ProtocolVersion})
	if env := readFrame(t, first, 5*time.Second); env.Type != TypeHelloOK {
		t.Fatalf("first handshake failed: %q", env.Type)
	}
	waitConnected(t, ts.Hub())

	// Second client for the same inbox.
	startFakeCLI(t, srv, func(req Request) *Response {
		return &Response{Status: 200, BodyB64: "second"}
	})

	// The displaced client is told why, rather than just vanishing.
	cl := expectClose(t, first, CodeReplaced)
	if !strings.Contains(cl.Reason, "another client") {
		t.Errorf("reason %q does not explain the replacement", cl.Reason)
	}

	// And the hub now routes to the winner. Retried because the first
	// client's deferred unregister races the second's register -- which is
	// exactly the situation the identity check in unregister exists for.
	var resp *Response
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		r, err := ts.Hub().Forward(ctx, goodID, Request{Method: "GET", Path: "/"})
		cancel()
		if err == nil {
			resp = r
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resp == nil {
		t.Fatal("the replacement tunnel never answered. Either the displaced " +
			"client's unregister removed its successor, or evicting the old " +
			"connection blocked the new one before its read loop started")
	}
	if resp.BodyB64 != "second" {
		t.Errorf("body = %q, want the second client's response", resp.BodyB64)
	}
}

// TestUnregisterIgnoresStaleClient pins the identity check directly, without
// depending on goroutine timing.
func TestUnregisterIgnoresStaleClient(t *testing.T) {
	h := NewHub()
	a := newClient(nil, quiet())
	b := newClient(nil, quiet())

	h.register(goodID, a)
	if old := h.register(goodID, b); old != a {
		t.Fatalf("register did not return the displaced client")
	}

	// a's deferred unregister now runs, late.
	h.unregister(goodID, a)

	if !h.Connected(goodID) {
		t.Fatal("the stale client's unregister removed its successor")
	}

	h.unregister(goodID, b)
	if h.Connected(goodID) {
		t.Fatal("unregister did not remove the current client")
	}
}

// TestDuplicateResponseIsHarmless covers a buggy or hostile client echoing
// one req_id twice. The second delivery must find nothing, rather than
// blocking the sole reader goroutine on a full channel buffer.
func TestDuplicateResponseIsHarmless(t *testing.T) {
	c := newClient(nil, quiet())
	ch := make(chan *Response, 1)
	c.pending["7"] = ch

	c.deliver(&Response{ReqID: "7", Status: 200})

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.deliver(&Response{ReqID: "7", Status: 500}) // must not block
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a duplicate response blocked deliver; the reader goroutine would be frozen")
	}

	if got := <-ch; got.Status != 200 {
		t.Errorf("status = %d, want the first response", got.Status)
	}
	if n := c.pendingCount(); n != 0 {
		t.Errorf("pending = %d, want 0", n)
	}
}

// TestSendAfterCloseFails: a client that has been closed must reject new work
// rather than registering an entry nobody will ever service.
func TestSendAfterCloseFails(t *testing.T) {
	c := newClient(nil, quiet())
	c.close()

	_, err := c.send(context.Background(), Request{Method: "GET"})
	if !errors.Is(err, ErrDisconnected) {
		t.Fatalf("err = %v, want ErrDisconnected", err)
	}
	if n := c.pendingCount(); n != 0 {
		t.Errorf("pending = %d after a rejected send, want 0", n)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	c := newClient(nil, quiet())
	c.close()
	c.close() // closing `done` twice would panic
}

// assertNoPending is the leak check. Every exit path in send() must remove its
// map entry; this is what catches the one that forgot.
func assertNoPending(t *testing.T, ts *Server) {
	t.Helper()
	ts.hub.mu.Lock()
	c := ts.hub.clients[goodID]
	ts.hub.mu.Unlock()
	if c == nil {
		return
	}
	// Small settle: the deferred forget runs just after Forward returns.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.pendingCount() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("%d entries left in the correlation map; every exit path must delete its own",
		c.pendingCount())
}
