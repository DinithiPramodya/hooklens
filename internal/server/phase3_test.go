package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DinithiPramodya/hooklens/internal/tunnel"
)

// The three-terminal test from PLAN.md, automated.
//
// The manual version is: a server in one terminal, `hooklens forward --to
// localhost:3000` in a second, an app in a third, and curl in a fourth. That
// is the right way to demo it and the wrong way to keep it working -- a
// demonstration nobody runs is a demonstration that silently stops passing.
//
// This runs the same thing with a REAL Client rather than a fake CLI, so the
// two halves built in units 18-24 are exercised together against real
// Postgres. It is the closest thing this repo has to an acceptance test.

// localApp is the third terminal: an app being developed.
func localApp(t *testing.T) (url string, hits *atomic.Int64, stop func()) {
	t.Helper()
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-From", "local-app")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"seen":` + string(body) + `}`))
	}))
	return srv.URL, &count, srv.Close
}

// realCLI is the second terminal: the actual tunnel client, not a stand-in.
func realCLI(t *testing.T, serverURL, slug, token, target string) (connected <-chan string, stop func()) {
	t.Helper()
	ch := make(chan string, 8)

	c, err := tunnel.NewClient(tunnel.ClientOptions{
		ServerURL: serverURL,
		Slug:      slug,
		Token:     token,
		Target:    target,
		Log:       quiet(),
		OnConnect: func(publicURL string) { ch <- publicURL },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = c.Run(ctx); close(done) }()

	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the CLI did not stop within 5s")
		}
	}
	t.Cleanup(stop)
	return ch, stop
}

// TestPhase3DoneWhen walks the four acceptance criteria from PLAN.md in one
// run, in order, against one inbox -- because they are not independent: each
// leaves the system in the state the next one starts from.
func TestPhase3DoneWhen(t *testing.T) {
	s, ep := storeServer(t, 0)
	// Shorten the forward deadline so the "local app hangs" leg does not
	// spend 30 seconds proving a point.
	s.ingest.SetForwardTimeout(500 * time.Millisecond)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	appURL, appHits, stopApp := localApp(t)
	connected, stopCLI := realCLI(t, srv.URL, ep.Slug, ep.Token, appURL)

	select {
	case url := <-connected:
		if !strings.Contains(url, ep.Slug) {
			t.Errorf("public URL %q does not contain the slug", url)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the CLI never connected")
	}
	waitTunnel(t, s, ep.ID)

	// ---- 1. the three-terminal test: a webhook reaches the app and its
	//         response reaches the sender ----
	t.Run("webhook reaches the local app", func(t *testing.T) {
		resp := post(t, srv, ep.Slug, "/hooks/stripe", `{"id":1}`)
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)

		if resp.StatusCode != 201 {
			t.Errorf("status = %d, want the app's 201", resp.StatusCode)
		}
		if got := resp.Header.Get("X-From"); got != "local-app" {
			t.Errorf("X-From = %q, want the app's header", got)
		}
		if string(body) != `{"seen":{"id":1}}` {
			t.Errorf("body = %q, want the app's echo", body)
		}
		if appHits.Load() != 1 {
			t.Errorf("the app was hit %d times, want 1", appHits.Load())
		}
		if d := deliveryFor(t, s, capturedID(t, resp)); d != "201" {
			t.Errorf("recorded delivery = %q, want 201", d)
		}
	})

	// ---- 2. killing the local app shows "unreachable" ----
	t.Run("a dead local app reports unreachable", func(t *testing.T) {
		stopApp()

		resp := post(t, srv, ep.Slug, "/hooks/stripe", `{"id":2}`)
		defer resp.Body.Close()

		// Still a 2xx: the capture is stored, and a non-2xx would make a
		// provider retry an event we already hold.
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		if h := resp.Header.Get("X-Hooklens-Forward"); h != "unreachable" {
			t.Errorf("X-Hooklens-Forward = %q, want unreachable", h)
		}
		// And it is visible, which is the actual criterion -- the UI reads
		// exactly this field.
		if d := deliveryFor(t, s, capturedID(t, resp)); d != "unreachable" {
			t.Errorf("recorded delivery = %q, want unreachable", d)
		}
	})

	// ---- 3. killing the tunnel mid-request unblocks ingest instantly ----
	t.Run("killing the tunnel mid-request does not hang", func(t *testing.T) {
		// A new app that never answers, so a request is genuinely in flight
		// when the tunnel dies.
		hang := make(chan struct{})
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-hang:
			case <-r.Context().Done():
			}
		}))
		// Registered BEFORE close(hang) so it runs AFTER it: defers are LIFO,
		// and httptest.Close blocks on active connections -- which the hung
		// request is until the channel is closed.
		defer slow.Close()
		defer close(hang)

		// The previous CLI is stopped first. Since the replacement fix, a
		// displaced client exits rather than fighting for the inbox -- so
		// starting a second one without stopping the first would simply kill
		// the first, which is correct behaviour and not what this leg tests.
		stopCLI()
		connected2, stopSlowCLI := realCLI(t, srv.URL, ep.Slug, ep.Token, slow.URL)
		// And stopped again before the next leg starts its own, for the same
		// reason.
		defer stopSlowCLI()
		select {
		case <-connected2:
		case <-time.After(10 * time.Second):
			t.Fatal("the replacement CLI never connected")
		}
		waitTunnel(t, s, ep.ID)

		done := make(chan time.Duration, 1)
		go func() {
			start := time.Now()
			resp := post(t, srv, ep.Slug, "/hooks/hang", `{"id":3}`)
			resp.Body.Close()
			done <- time.Since(start)
		}()

		// Let it get in flight, then pull the tunnel out from under it.
		time.Sleep(200 * time.Millisecond)
		s.tunnel.Hub().DropForTest(ep.ID)

		select {
		case elapsed := <-done:
			// The forward deadline is 500ms and the provider's patience is
			// not the point: the assertion is that ingest was woken rather
			// than waiting anything out.
			if elapsed > 5*time.Second {
				t.Errorf("ingest took %v to unblock after the tunnel died", elapsed)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("ingest hung after the tunnel was killed")
		}
	})

	// ---- 4. the URL survives a drop, automatically ----
	t.Run("the same URL comes back by itself", func(t *testing.T) {
		app2URL, app2Hits, stopApp2 := localApp(t)
		defer stopApp2()

		conn, _ := realCLI(t, srv.URL, ep.Slug, ep.Token, app2URL)
		var first string
		select {
		case first = <-conn:
		case <-time.After(10 * time.Second):
			t.Fatal("the CLI never connected")
		}
		waitTunnel(t, s, ep.ID)

		// Drop it the way a network flap would: no close frame.
		s.tunnel.Hub().DropForTest(ep.ID)

		select {
		case again := <-conn:
			if again != first {
				t.Errorf("URL changed across a reconnect: %q then %q", first, again)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("the CLI did not come back on its own")
		}
		waitTunnel(t, s, ep.ID)

		// And it works again, which a reconnect that only registers would not.
		before := app2Hits.Load()
		resp := post(t, srv, ep.Slug, "/hooks/after", `{"id":4}`)
		defer resp.Body.Close()
		if resp.StatusCode != 201 {
			t.Errorf("status = %d after reconnect, want the app's 201", resp.StatusCode)
		}
		if app2Hits.Load() != before+1 {
			t.Error("the app was not reached after the reconnect")
		}
	})

	stopCLI()
}

// deliveryFor returns the recorded outcome as the UI would read it: the
// status if delivered, otherwise the error code.
func deliveryFor(t *testing.T, s *Server, id string) string {
	t.Helper()
	// Polled: RecordForward happens before the response is written, but the
	// row is read on a different connection.
	deadline := time.Now().Add(3 * time.Second)
	for {
		req, err := s.store.GetRequest(t.Context(), id)
		if err != nil {
			t.Fatalf("GetRequest: %v", err)
		}
		switch {
		case req.ForwardStatus != nil:
			return strconv.Itoa(*req.ForwardStatus)
		case req.ForwardError != nil:
			return *req.ForwardError
		}
		if time.Now().After(deadline) {
			return "(nothing recorded)"
		}
		time.Sleep(20 * time.Millisecond)
	}
}
