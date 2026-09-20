package server

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/DinithiPramodya/hooklens/internal/config"
	"github.com/DinithiPramodya/hooklens/internal/store"
	"time"
)

// TestResponseControllerReachesThroughMiddleware is a regression test for a bug
// that made SSE impossible and was invisible until something needed to flush.
//
// statusRecorder wraps every response for logging. http.ResponseController
// reaches optional interfaces (Flusher, SetWriteDeadline) by walking Unwrap();
// a wrapper without it is a wall, and the controller returns ErrUnsupported for
// everything. The failure mode is the nastiest available: handlers run, writes
// report success, and the client receives nothing.
func TestResponseControllerReachesThroughMiddleware(t *testing.T) {
	var flushErr, deadlineErr error

	// The handler runs on the server's goroutine and this test reads its
	// results from another. A completed http.Get does NOT mean the handler has
	// returned -- the client has the response as soon as it is flushed, while
	// the handler may still be executing. Reading the variables at that point
	// is a data race, and the race detector caught exactly that here.
	//
	// Closing this channel after the writes, and receiving from it before the
	// reads, is the happens-before edge that makes the handoff legal.
	done := make(chan struct{})

	h := withRecover(quiet(), withRequestLog(quiet(), http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			defer close(done)
			rc := http.NewResponseController(w)
			deadlineErr = rc.SetWriteDeadline(time.Time{})
			_, _ = w.Write([]byte("x"))
			flushErr = rc.Flush()
		})))

	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not finish")
	}

	if deadlineErr != nil {
		t.Errorf("SetWriteDeadline through middleware: %v -- statusRecorder needs Unwrap()", deadlineErr)
	}
	if flushErr != nil {
		t.Errorf("Flush through middleware: %v -- statusRecorder needs Unwrap()", flushErr)
	}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// sseTestServer runs a real HTTP server, not httptest.NewRecorder.
//
// A recorder cannot support SetWriteDeadline and buffers everything, so it
// would pass tests that a browser fails. Streaming has to be tested over a
// socket.
func sseTestServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// TestSSEWireFormat pins the format itself against a bare handler, with no
// database in the way.
func TestSSEWireFormat(t *testing.T) {
	h := withRequestLog(quiet(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse, err := newSSEWriter(w)
		if err != nil {
			t.Errorf("newSSEWriter: %v", err)
			return
		}
		_ = sse.event("capture", `{"id":"abc"}`)
		_ = sse.comment("keepalive")
		// A payload containing newlines must become several data: lines.
		_ = sse.event("multi", "first\nsecond")
		_ = sse.event("", "unnamed")
	}))

	srv := sseTestServer(t, h)
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q -- a buffering proxy would hold every event", got)
	}

	body, _ := io.ReadAll(resp.Body)
	want := "retry: 3000\n\n" +
		"event: capture\ndata: {\"id\":\"abc\"}\n\n" +
		": keepalive\n\n" +
		"event: multi\ndata: first\ndata: second\n\n" +
		"data: unnamed\n\n"

	if string(body) != want {
		t.Errorf("wire format mismatch\n got %q\nwant %q", body, want)
	}
}

// TestSSEFlushesBeforeHandlerReturns is the property the whole feature rests
// on. The handler below never returns during the read, so anything the client
// sees must have been flushed.
func TestSSEFlushesBeforeHandlerReturns(t *testing.T) {
	release := make(chan struct{})

	h := withRequestLog(quiet(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse, err := newSSEWriter(w)
		if err != nil {
			t.Errorf("newSSEWriter: %v", err)
			return
		}
		_ = sse.event("first", "now")
		<-release // hold the handler open
	}))

	srv := sseTestServer(t, h)
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Read only as far as the first event. If writes were not flushed this
	// blocks until the 5s context deadline and the test fails.
	br := bufio.NewReader(resp.Body)
	var got strings.Builder
	for i := 0; i < 4; i++ {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read %d: %v -- nothing was flushed", i, err)
		}
		got.WriteString(line)
	}
	if !strings.Contains(got.String(), "event: first") {
		t.Errorf("did not receive the first event before the handler returned:\n%q", got.String())
	}
}

// TestSSESurvivesWriteTimeout proves the SetWriteDeadline call earns its place.
//
// The server is given a WriteTimeout shorter than the test, which without the
// per-connection deadline clear would cut the stream mid-flight.
func TestSSESurvivesWriteTimeout(t *testing.T) {
	h := withRequestLog(quiet(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse, err := newSSEWriter(w)
		if err != nil {
			t.Errorf("newSSEWriter: %v", err)
			return
		}
		for i := 0; i < 5; i++ {
			if err := sse.comment("tick"); err != nil {
				return
			}
			time.Sleep(150 * time.Millisecond)
		}
	}))

	srv := httptest.NewUnstartedServer(h)
	srv.Config.WriteTimeout = 200 * time.Millisecond // far shorter than the ~750ms stream
	srv.Start()
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("stream was cut: %v -- SetWriteDeadline is not taking effect", err)
	}
	if n := strings.Count(string(body), ": tick"); n != 5 {
		t.Errorf("received %d ticks, want 5 -- the write deadline cut the stream", n)
	}
}

// storeServer builds a Server backed by a real database, or skips.
//
// handleStream authenticates before it streams, so unlike the routes in
// routes_test.go it genuinely needs the store -- a nil one panics into a 500
// and the test asserts the wrong thing. Same skip-if-absent trade as
// internal/store: honest, and CI always has Postgres.
func storeServer(t *testing.T, heartbeat time.Duration) (*Server, *store.NewEndpoint) {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://hooklens:hooklens@localhost:5432/hooklens?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Skipf("no database reachable (%v) -- run `docker compose up -d`", err)
	}
	t.Cleanup(st.Close)

	ep, err := st.CreateEndpoint(t.Context(), "stream test")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	s := New(config.Config{Env: "dev", Addr: ":0", BaseDomain: "localhost"}, quiet(), st)
	if heartbeat > 0 {
		s.heartbeat = heartbeat
	}
	return s, ep
}

// TestStreamRequiresAuth: the stream is as sensitive as the request list, and
// must refuse identically -- including refusing to become a stream at all.
func TestStreamRequiresAuth(t *testing.T) {
	s, ep := storeServer(t, 0)

	for _, tc := range []struct{ name, token string }{
		{"no token", ""},
		{"wrong token", "not-the-token"},
	} {
		h := map[string]string{}
		if tc.token != "" {
			h["Authorization"] = "Bearer " + tc.token
		}
		resp := do(t, s, "GET", "/api/endpoints/"+ep.Slug+"/stream", h)
		resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", tc.name, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
			t.Errorf("%s: an unauthenticated request was upgraded to a stream", tc.name)
		}
	}
}

// TestStreamEndToEnd opens a real authenticated stream over a socket and reads
// the connected event and a heartbeat, then asserts the server notices when the
// client disconnects.
func TestStreamEndToEnd(t *testing.T) {
	s, ep := storeServer(t, 60*time.Millisecond)

	srv := sseTestServer(t, s)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/endpoints/"+ep.Slug+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+ep.Token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}

	br := bufio.NewReader(resp.Body)
	var seen strings.Builder
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		seen.WriteString(line)
		// Stop once we have the initial event AND at least one heartbeat,
		// which together prove the stream is live rather than merely opened.
		if strings.Contains(seen.String(), "event: connected") &&
			strings.Contains(seen.String(), ": keepalive") {
			break
		}
	}

	got := seen.String()
	if !strings.Contains(got, "retry: 3000") {
		t.Errorf("no retry directive -- clients would use their own default:\n%q", got)
	}
	if !strings.Contains(got, "event: connected") {
		t.Errorf("no connected event:\n%q", got)
	}
	if !strings.Contains(got, ep.Slug) {
		t.Errorf("connected event did not name the inbox:\n%q", got)
	}
	if !strings.Contains(got, ": keepalive") {
		t.Errorf("no heartbeat within 5s at a 60ms interval:\n%q", got)
	}
}

// TestStreamStopsWhenClientLeaves pins the goroutine-lifetime property: the
// handler must return when the client disconnects, or every closed tab leaks a
// goroutine and a database-pool-adjacent connection for the life of the process.
func TestStreamStopsWhenClientLeaves(t *testing.T) {
	s, ep := storeServer(t, 30*time.Millisecond)
	srv := sseTestServer(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/endpoints/"+ep.Slug+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+ep.Token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	// Read one line so the handler is definitely inside its loop.
	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("read: %v", err)
	}

	before := runtime.NumGoroutine()
	cancel() // the client goes away
	resp.Body.Close()

	// The handler observes r.Context() cancellation and returns. Poll rather
	// than sleep a fixed amount, so this is not flaky on a slow machine.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("goroutine count did not fall after the client left (%d -> %d)",
		before, runtime.NumGoroutine())
}
