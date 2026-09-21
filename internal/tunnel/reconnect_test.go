package tunnel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// startClient runs a real Client against a real tunnel server and reports
// each successful connection on the returned channel.
func startClient(t *testing.T, srv *httptest.Server, slug, token string) (<-chan string, <-chan error) {
	t.Helper()

	connects := make(chan string, 16)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	t.Cleanup(app.Close)

	c, err := NewClient(ClientOptions{
		ServerURL: srv.URL,
		Slug:      slug,
		Token:     token,
		Target:    app.URL,
		Log:       quiet(),
		OnConnect: func(publicURL string) { connects <- publicURL },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// A separate signal from `done`, because a test that reads `done` would
	// otherwise consume the only value and leave the cleanup below blocking
	// on a channel nothing will send to again. Closing a channel is
	// broadcast; sending is not.
	stopped := make(chan struct{})
	go func() {
		done <- c.Run(ctx)
		close(stopped)
	}()

	// Cancel AND WAIT. Cancelling alone leaves the Run goroutine alive for a
	// moment, and a client still mid-reconnect outlives its own test. Together
	// with per-test inboxes, that is what keeps one test from displacing the
	// next one's tunnel.
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Error("client did not stop within 5s of cancellation")
		}
	})

	return connects, done
}

func waitConnect(t *testing.T, connects <-chan string, within time.Duration, what string) {
	t.Helper()
	select {
	case <-connects:
	case <-time.After(within):
		t.Fatalf("no %s within %v", what, within)
	}
}

// TestClientReconnectsAfterDrop is failure mode 6: the network flaps and the
// CLI comes back on the SAME URL without being restarted.
func TestClientReconnectsAfterDrop(t *testing.T) {
	ts, srv, _ := newTestServer(t, Options{})
	slug, endpointID := uniqueInbox(t)

	connects, _ := startClient(t, srv, slug, goodToken)
	waitConnect(t, connects, 5*time.Second, "initial connection")
	waitConnected(t, ts.Hub(), endpointID)

	// Kill the connection from the server side, the way a flaky network
	// would -- no close frame, no warning.
	ts.hub.mu.Lock()
	victim := ts.hub.clients[endpointID]
	ts.hub.mu.Unlock()
	if victim == nil {
		t.Fatal("no client registered to drop")
	}
	victim.conn.CloseNow()

	// The first retry ceiling is backoffBase, so this should be quick. The
	// generous bound is for CI, not for the implementation.
	waitConnect(t, connects, 10*time.Second, "reconnection")

	// And the tunnel works again afterwards -- a reconnect that registers but
	// cannot forward would be worse than none.
	waitConnected(t, ts.Hub(), endpointID)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	resp, err := ts.Hub().Forward(ctx, endpointID, Request{Method: "POST", Path: "/after-reconnect"})
	if err != nil {
		t.Fatalf("forward after reconnect: %v", err)
	}
	if resp.Status != 204 {
		t.Errorf("status = %d, want 204 from the local app", resp.Status)
	}
}

// TestClientReconnectsRepeatedly: a flapping network, not a single blip.
// This is where resetting the backoff on connect rather than on a LASTING
// connection would show up as an ever-growing delay or a hot loop.
func TestClientReconnectsRepeatedly(t *testing.T) {
	ts, srv, _ := newTestServer(t, Options{})
	slug, endpointID := uniqueInbox(t)

	connects, _ := startClient(t, srv, slug, goodToken)
	waitConnect(t, connects, 5*time.Second, "initial connection")

	for i := range 3 {
		waitConnected(t, ts.Hub(), endpointID)
		ts.hub.mu.Lock()
		victim := ts.hub.clients[endpointID]
		ts.hub.mu.Unlock()
		if victim == nil {
			t.Fatalf("drop %d: no client registered", i+1)
		}
		victim.conn.CloseNow()
		waitConnect(t, connects, 15*time.Second, "reconnection after drop")
	}
}

// TestClientStopsOnPermanentError: a rejected token will never start working,
// and retrying it forever is a busy-wait that also looks like a credential
// attack. Run must return instead.
func TestClientStopsOnPermanentError(t *testing.T) {
	_, srv, _ := newTestServer(t, Options{})
	slug, _ := uniqueInbox(t)

	_, done := startClient(t, srv, slug, "the-wrong-token")

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil for a rejected token; it should report why it gave up")
		}
		// The message a user sees has to be the server's, not a socket error.
		if !strings.Contains(err.Error(), "inbox") && !strings.Contains(err.Error(), "token") {
			t.Errorf("error = %q, which does not explain the rejection", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run was still retrying a rejected token after 5s")
	}
}

// TestClientStopsOnCancel: ctrl-c during a backoff sleep must not wait it out.
func TestClientStopsOnCancel(t *testing.T) {
	// No server at all, so every attempt fails and the client is in its
	// backoff loop.
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer app.Close()

	c, err := NewClient(ClientOptions{
		// A port nothing is listening on.
		ServerURL: "http://127.0.0.1:1",
		Slug:      goodSlug,
		Token:     goodToken,
		Target:    app.URL,
		Log:       quiet(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// Let it fail at least once and enter the sleep.
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v on cancellation, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return on cancellation; the backoff sleep is not cancellable")
	}
}

// TestClientSurvivesServerRestart is the realistic version: the server goes
// away entirely and comes back, which is a deploy.
func TestClientSurvivesServerRestart(t *testing.T) {
	slug, endpointID := uniqueInbox(t)
	ctx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()

	// A handler that can be swapped, so the same listener address survives
	// the "restart" -- the client must reconnect to where it was told, and
	// httptest picks a fresh port for a fresh server.
	var mu sync.Mutex
	down := false
	ts := &Server{
		log: quiet(), auth: testAuth,
		publicURL:        func(slug string) string { return "https://" + slug + ".example.test/" },
		baseCtx:          ctx,
		hub:              NewHub(),
		handshakeTimeout: defaultHandshakeTimeout,
		pingInterval:     defaultPingInterval,
		pongTimeout:      defaultPongTimeout,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		unavailable := down
		mu.Unlock()
		if unavailable {
			http.Error(w, "down for maintenance", http.StatusServiceUnavailable)
			return
		}
		ts.Handle(w, r)
	}))
	defer srv.Close()

	connects, _ := startClient(t, srv, slug, goodToken)
	waitConnect(t, connects, 5*time.Second, "initial connection")

	// "Restart": refuse upgrades, drop the live connection, then recover.
	mu.Lock()
	down = true
	mu.Unlock()

	ts.hub.mu.Lock()
	victim := ts.hub.clients[endpointID]
	ts.hub.mu.Unlock()
	if victim != nil {
		victim.conn.CloseNow()
	}

	time.Sleep(600 * time.Millisecond) // long enough for a failed attempt or two

	mu.Lock()
	down = false
	mu.Unlock()

	waitConnect(t, connects, 20*time.Second, "reconnection after the server came back")
}
