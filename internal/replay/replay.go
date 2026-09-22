package replay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ReplayHeader marks a replayed request.
//
// Not optional. A replay is byte-identical to the original downstream, so
// without a marker a developer's own logs cannot distinguish "the provider
// sent this twice" from "I resent it" -- and telling those apart is the
// entire reason they are using this tool.
const ReplayHeader = "X-Hooklens-Replay"

// maxResponseBytes bounds what we read back from a replay target.
const maxResponseBytes = 1 << 20

// Request is what to send.
type Request struct {
	Method  string
	Path    string
	Query   string
	Headers []Header
	Body    []byte
}

// Header is one header. Its own type here for the same reason every other
// package in this repo declares one: this is a leaf, and a replayer that
// imports the storage layer cannot be tested without it.
type Header struct {
	Name  string
	Value string
}

// Response is what came back.
type Response struct {
	Status  int
	Headers []Header
	Body    []byte
	Elapsed time.Duration
}

// hopByHop describes a single transport hop and must not be forwarded.
//
// Duplicated from internal/ingest and internal/tunnel rather than shared.
// Three copies of nine strings is less coupling than a utility package that
// every layer depends on, and the three lists are free to diverge -- this
// one also drops Host, which the others keep.
func hopByHop(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade", "content-length", "host":
		return true
	}
	return false
}

// ToURL replays a request to an arbitrary destination.
//
// The destination is guarded at the dial -- see SafeClient -- so this
// function does not need to trust anything about the URL beyond its shape.
func ToURL(ctx context.Context, client *http.Client, target string, req Request) (*Response, error) {
	u, err := parseHTTPURL(target)
	if err != nil {
		return nil, err
	}

	// The capture's path is appended to the target's, so replaying to
	// `http://example.test/api` sends `/api/hooks/stripe`. Matches the
	// tunnel client's behaviour, which a user will reasonably expect to be
	// the same thing.
	u.Path = strings.TrimSuffix(u.Path, "/") + req.Path
	if req.Query != "" {
		u.RawQuery = req.Query
	}

	hreq, err := http.NewRequestWithContext(ctx, req.Method, u.String(), bytes.NewReader(req.Body))
	if err != nil {
		return nil, fmt.Errorf("build replay request: %w", err)
	}
	for _, h := range req.Headers {
		if hopByHop(h.Name) {
			continue
		}
		hreq.Header.Add(h.Name, h.Value)
	}
	hreq.Header.Set(ReplayHeader, "1")

	start := time.Now()
	resp, err := client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read replay response: %w", err)
	}

	out := &Response{Status: resp.StatusCode, Body: body, Elapsed: time.Since(start)}
	for name, values := range resp.Header {
		for _, v := range values {
			out.Headers = append(out.Headers, Header{Name: name, Value: v})
		}
	}
	return out, nil
}

// parseHTTPURL accepts only http and https, and requires a host.
//
// Restricting the scheme is not pedantry. Go's transport does not speak
// file:// or gopher://, but a permissive parse here would let a target
// through to code that assumes it was checked -- and the set of URL schemes
// something downstream might one day handle is not a set worth betting on.
func parseHTTPURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("no target URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("bad target URL: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("target scheme must be http or https, got %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("target URL has no host")
	}
	return u, nil
}
