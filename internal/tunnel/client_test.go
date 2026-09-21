package tunnel

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseTarget(t *testing.T) {
	tests := []struct {
		in       string
		wantHost string
		wantPath string
		wantErr  bool
	}{
		// The form people actually type. url.Parse reads this as scheme
		// "localhost" with opaque path "3000" -- valid and completely wrong --
		// which is why the missing scheme has to be detected.
		{in: "localhost:3000", wantHost: "localhost:3000"},
		{in: "127.0.0.1:8080", wantHost: "127.0.0.1:8080"},
		{in: "http://localhost:3000", wantHost: "localhost:3000"},
		{in: "https://example.test", wantHost: "example.test"},
		{in: "localhost:3000/api", wantHost: "localhost:3000", wantPath: "/api"},
		{in: "", wantErr: true},
	}

	for _, tc := range tests {
		got, err := parseTarget(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseTarget(%q) = %v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTarget(%q): %v", tc.in, err)
			continue
		}
		if got.Host != tc.wantHost {
			t.Errorf("parseTarget(%q).Host = %q, want %q", tc.in, got.Host, tc.wantHost)
		}
		if got.Path != tc.wantPath {
			t.Errorf("parseTarget(%q).Path = %q, want %q", tc.in, got.Path, tc.wantPath)
		}
	}
}

func TestWebsocketURL(t *testing.T) {
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "http://localhost:8080", want: "ws://localhost:8080/api/tunnel"},
		{in: "https://hooklens.dev", want: "wss://hooklens.dev/api/tunnel"},
		{in: "localhost:8080", want: "ws://localhost:8080/api/tunnel"},
		// A trailing slash must not produce a double slash in the path.
		{in: "http://localhost:8080/", want: "ws://localhost:8080/api/tunnel"},
		{in: "ftp://nope", wantErr: true},
	}
	for _, tc := range tests {
		got, err := websocketURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("websocketURL(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("websocketURL(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("websocketURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestShortError(t *testing.T) {
	// The whole point: the actionable clause is the last one, and nobody
	// reads past the first.
	err := errors.New(`Get "http://localhost:3000/hook": dial tcp 127.0.0.1:3000: connect: connection refused`)
	if got := ShortError(err); got != "connection refused" {
		t.Errorf("ShortError = %q, want %q", got, "connection refused")
	}
	if got := ShortError(errors.New("plain")); got != "plain" {
		t.Errorf("ShortError(plain) = %q", got)
	}
}

// newTestClient builds a Client pointed at a local test server, without
// touching a WebSocket. callLocal is the half worth testing in isolation.
func newTestClient(t *testing.T, target string) *Client {
	t.Helper()
	c, err := NewClient(ClientOptions{Target: target, ServerURL: "http://unused", Log: quiet()})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestCallLocalRelaysTheRequest(t *testing.T) {
	type seen struct {
		method, path, query, host, fwdHost, body string
		header                                   http.Header
	}
	var got seen

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = seen{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			host: r.Host, fwdHost: r.Header.Get("X-Forwarded-Host"),
			body: string(body), header: r.Header.Clone(),
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Add("Set-Cookie", "a=1")
		w.Header().Add("Set-Cookie", "b=2")
		w.WriteHeader(201)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer app.Close()

	c := newTestClient(t, app.URL)
	resp := c.callLocal(t.Context(), Request{
		Method: "POST",
		Path:   "/hooks/stripe",
		Query:  "a=1&b=2",
		Headers: []Header{
			{Name: "Content-Type", Value: "application/json"},
			{Name: "X-Signature", Value: "abc"},
			// The provider addressed OUR domain.
			{Name: "Host", Value: "abc123.hooklens.dev"},
			// Describes a connection that ended at our server.
			{Name: "Connection", Value: "keep-alive"},
			// Would describe a different body than the one we send.
			{Name: "Content-Length", Value: "999"},
		},
		BodyB64: base64.StdEncoding.EncodeToString([]byte(`{"hi":1}`)),
	})

	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	// What the local app received.
	if got.method != "POST" || got.path != "/hooks/stripe" || got.query != "a=1&b=2" {
		t.Errorf("app saw %s %s?%s", got.method, got.path, got.query)
	}
	if got.body != `{"hi":1}` {
		t.Errorf("app saw body %q", got.body)
	}
	if got.header.Get("X-Signature") != "abc" {
		t.Error("the signature header did not survive; Phase 4 depends on it")
	}
	// Host is the target, not the provider's -- frameworks that validate Host
	// would reject the request otherwise.
	if strings.Contains(got.host, "hooklens.dev") {
		t.Errorf("Host = %q; the provider's Host was forwarded verbatim", got.host)
	}
	if got.fwdHost != "abc123.hooklens.dev" {
		t.Errorf("X-Forwarded-Host = %q, want the original Host preserved", got.fwdHost)
	}
	if got.header.Get("Connection") != "" {
		t.Error("a hop-by-hop header reached the local app")
	}
	if cl := got.header.Get("Content-Length"); cl == "999" {
		t.Error("the original Content-Length was forwarded; it describes a different body")
	}

	// What came back.
	if resp.Status != 201 {
		t.Errorf("status = %d, want 201", resp.Status)
	}
	body, err := base64.StdEncoding.DecodeString(resp.BodyB64)
	if err != nil || string(body) != `{"ok":true}` {
		t.Errorf("body = %q (err %v)", body, err)
	}
	var cookies int
	for _, h := range resp.Headers {
		if strings.EqualFold(h.Name, "Set-Cookie") {
			cookies++
		}
		if isHopByHopHeader(h.Name) {
			t.Errorf("hop-by-hop %q came back in the response frame", h.Name)
		}
	}
	if cookies != 2 {
		t.Errorf("got %d Set-Cookie headers back, want 2", cookies)
	}
}

// TestCallLocalUnreachable is failure mode 2 from the client's side. Nothing
// is running on the target port, and the result must be an Error rather than
// a status -- the server relies on that distinction.
func TestCallLocalUnreachable(t *testing.T) {
	// A server started and immediately closed, so the port is almost
	// certainly free and certainly not listening.
	app := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := app.URL
	app.Close()

	c := newTestClient(t, addr)
	resp := c.callLocal(t.Context(), Request{Method: "POST", Path: "/hook"})

	if resp.Error == "" {
		t.Fatal("reaching a dead port produced no Error")
	}
	if resp.Status != 0 {
		t.Errorf("status = %d, want 0 -- a status would be read as an app response", resp.Status)
	}
	// The message is what a developer reads, so it has to name the problem.
	if !strings.Contains(strings.ToLower(resp.Error), "refus") &&
		!strings.Contains(strings.ToLower(resp.Error), "connect") {
		t.Errorf("Error = %q, which does not explain that the app was unreachable", resp.Error)
	}
}

// TestCallLocalDoesNotFollowRedirects: a 302 is a real answer that the
// provider should see. Go's http.Client follows redirects by default, which
// would silently replace the developer's response.
func TestCallLocalDoesNotFollowRedirects(t *testing.T) {
	var elsewhereHit bool
	mux := http.NewServeMux()
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, r *http.Request) {
		elsewhereHit = true
		w.WriteHeader(200)
	})
	mux.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	app := httptest.NewServer(mux)
	defer app.Close()

	c := newTestClient(t, app.URL)
	resp := c.callLocal(t.Context(), Request{Method: "POST", Path: "/hook"})

	if resp.Status != http.StatusFound {
		t.Errorf("status = %d, want 302 relayed unchanged", resp.Status)
	}
	if elsewhereHit {
		t.Error("the redirect was followed; the provider would never see the 302")
	}
	var location string
	for _, h := range resp.Headers {
		if strings.EqualFold(h.Name, "Location") {
			location = h.Value
		}
	}
	if location != "/elsewhere" {
		t.Errorf("Location = %q, want it relayed", location)
	}
}

// TestCallLocalDoesNotDecompress: Go's transport adds Accept-Encoding: gzip
// and silently decompresses, which would leave the relayed bytes not matching
// the Content-Encoding header that travels with them.
func TestCallLocalDoesNotDecompress(t *testing.T) {
	var sawAcceptEncoding string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAcceptEncoding = r.Header.Get("Accept-Encoding")
		w.WriteHeader(200)
	}))
	defer app.Close()

	c := newTestClient(t, app.URL)
	c.callLocal(t.Context(), Request{Method: "GET", Path: "/"})

	if sawAcceptEncoding != "" {
		t.Errorf("Accept-Encoding = %q; the transport added one the provider did not send",
			sawAcceptEncoding)
	}
}

// TestCallLocalRespectsTargetPathPrefix: --to localhost:3000/api.
func TestCallLocalRespectsTargetPathPrefix(t *testing.T) {
	var path string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
	}))
	defer app.Close()

	c := newTestClient(t, app.URL+"/api")
	c.callLocal(t.Context(), Request{Method: "POST", Path: "/hook"})

	if path != "/api/hook" {
		t.Errorf("path = %q, want the target prefix applied", path)
	}
}

// TestCallLocalTimesOut: a hung app must produce an Error, not a hang.
func TestCallLocalTimesOut(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer app.Close()

	c := newTestClient(t, app.URL)
	// Shrink the client's own timeout rather than waiting 25 seconds.
	c.http.Timeout = 150 * time.Millisecond

	start := time.Now()
	resp := c.callLocal(context.Background(), Request{Method: "POST", Path: "/hang"})
	elapsed := time.Since(start)

	if resp.Error == "" {
		t.Fatal("a hung app produced no Error")
	}
	if resp.Status != 0 {
		t.Errorf("status = %d, want 0", resp.Status)
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %v; the client timeout did not fire", elapsed)
	}
}

// TestCallLocalRejectsBadBase64 covers a corrupt frame from the server.
func TestCallLocalRejectsBadBase64(t *testing.T) {
	c := newTestClient(t, "localhost:1")
	resp := c.callLocal(t.Context(), Request{Method: "POST", Path: "/", BodyB64: "!!!"})
	if resp.Error == "" {
		t.Fatal("an undecodable body produced no Error")
	}
	if !strings.Contains(resp.Error, "body") {
		t.Errorf("Error = %q, which does not say the body was the problem", resp.Error)
	}
}
