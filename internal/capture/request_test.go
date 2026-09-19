package capture

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFromHTTPBody(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		maxBody   int64
		wantBody  string
		wantTrunc bool
	}{
		{"empty", "", 10, "", false},
		{"under the limit", "hello", 10, "hello", false},
		{"exactly at the limit", "0123456789", 10, "0123456789", false},
		{"one byte over", "0123456789x", 10, "0123456789", true},
		{"far over", strings.Repeat("x", 5000), 10, strings.Repeat("x", 10), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/webhook", strings.NewReader(tt.body))
			got, err := FromHTTP(r, tt.maxBody)
			if err != nil {
				t.Fatalf("FromHTTP: %v", err)
			}
			if string(got.Body) != tt.wantBody {
				t.Errorf("Body = %q, want %q", got.Body, tt.wantBody)
			}
			if got.Truncated != tt.wantTrunc {
				t.Errorf("Truncated = %v, want %v", got.Truncated, tt.wantTrunc)
			}
			if int64(len(got.Body)) > tt.maxBody {
				t.Errorf("kept %d bytes, over the %d limit", len(got.Body), tt.maxBody)
			}
		})
	}
}

// TestFromHTTPBodyIsRaw is the test that protects Phase 4. If anyone ever
// "helpfully" parses and re-serialises the body on the way in, the bytes change
// and every HMAC verification breaks. This pins the bytes exactly.
func TestFromHTTPBodyIsRaw(t *testing.T) {
	// Deliberately ugly JSON: duplicate key, odd spacing, unsorted keys,
	// trailing newline. All of it must survive.
	raw := "{\"b\":1,   \"a\":2,\n\"b\":3}\n"

	r := httptest.NewRequest("POST", "/webhook", strings.NewReader(raw))
	got, err := FromHTTP(r, DefaultMaxBody)
	if err != nil {
		t.Fatalf("FromHTTP: %v", err)
	}
	if string(got.Body) != raw {
		t.Errorf("body was altered\n got %q\nwant %q", got.Body, raw)
	}
}

// TestFromHTTPBodyIsNotUTF8 -- a body is bytes, not a string. Protobuf, gzip
// and image payloads are all legal webhook bodies.
func TestFromHTTPBodyIsNotUTF8(t *testing.T) {
	raw := []byte{0x00, 0xff, 0xfe, 0x1f, 0x8b, 0x08, 0x00}

	r := httptest.NewRequest("POST", "/webhook", strings.NewReader(string(raw)))
	got, err := FromHTTP(r, DefaultMaxBody)
	if err != nil {
		t.Fatalf("FromHTTP: %v", err)
	}
	if string(got.Body) != string(raw) {
		t.Errorf("Body = %v, want %v", got.Body, raw)
	}
}

func TestFromHTTPHeaders(t *testing.T) {
	r := httptest.NewRequest("POST", "/webhook", strings.NewReader(""))
	r.Host = "a7f3.hooklens.dev"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Add("X-Custom", "first")
	r.Header.Add("X-Custom", "second")
	r.Header.Set("Zebra", "z")
	r.Header.Set("Alpha", "a")

	got, err := FromHTTP(r, DefaultMaxBody)
	if err != nil {
		t.Fatalf("FromHTTP: %v", err)
	}

	want := []Header{
		{"Alpha", "a"},
		{"Content-Type", "application/json"},
		{"Host", "a7f3.hooklens.dev"},
		{"X-Custom", "first"},  // multi-value preserved...
		{"X-Custom", "second"}, // ...in arrival order
		{"Zebra", "z"},
	}
	if len(got.Headers) != len(want) {
		t.Fatalf("got %d headers, want %d: %+v", len(got.Headers), len(want), got.Headers)
	}
	for i := range want {
		if got.Headers[i] != want[i] {
			t.Errorf("header %d = %+v, want %+v", i, got.Headers[i], want[i])
		}
	}
}

// TestFromHTTPRestoresHost pins the gotcha: net/http moves Host out of the
// header map, so a naive range over r.Header silently loses the one header that
// decided which inbox the request landed in.
func TestFromHTTPRestoresHost(t *testing.T) {
	r := httptest.NewRequest("POST", "/webhook", nil)
	r.Host = "a7f3.hooklens.dev"

	if _, inMap := r.Header["Host"]; inMap {
		t.Fatal("precondition changed: net/http now keeps Host in r.Header")
	}

	got, err := FromHTTP(r, DefaultMaxBody)
	if err != nil {
		t.Fatalf("FromHTTP: %v", err)
	}
	var found string
	for _, h := range got.Headers {
		if h.Name == "Host" {
			found = h.Value
		}
	}
	if found != "a7f3.hooklens.dev" {
		t.Errorf("Host header = %q, want it restored", found)
	}
}

func TestFromHTTPMetadata(t *testing.T) {
	r := httptest.NewRequest("PUT", "/a/b?x=1&y=2", strings.NewReader("body"))
	r.RemoteAddr = "203.0.113.9:54321"

	got, err := FromHTTP(r, DefaultMaxBody)
	if err != nil {
		t.Fatalf("FromHTTP: %v", err)
	}

	if got.Method != "PUT" {
		t.Errorf("Method = %q", got.Method)
	}
	if got.Path != "/a/b" {
		t.Errorf("Path = %q, want /a/b (query must not be in Path)", got.Path)
	}
	if got.Query != "x=1&y=2" {
		t.Errorf("Query = %q", got.Query)
	}
	if got.SourceIP.String() != "203.0.113.9" {
		t.Errorf("SourceIP = %q", got.SourceIP)
	}
	if got.ContentType() != "" {
		t.Errorf("ContentType = %q, want empty", got.ContentType())
	}
	if got.ReceivedAt.IsZero() {
		t.Error("ReceivedAt not set")
	}
}

func TestSourceIP(t *testing.T) {
	tests := []struct{ in, want string }{
		{"203.0.113.9:54321", "203.0.113.9"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"[::1]:8080", "::1"},
		{"203.0.113.9", "203.0.113.9"}, // no port
		{"garbage", "invalid IP"},      // netip zero value stringifies to this
		{"", "invalid IP"},
	}
	for _, tt := range tests {
		if got := sourceIP(tt.in).String(); got != tt.want {
			t.Errorf("sourceIP(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// errReader fails partway through, like a client hanging up mid-body.
type errReader struct {
	data []byte
	n    int
}

func (e *errReader) Read(p []byte) (int, error) {
	if e.n >= len(e.data) {
		return 0, errors.New("connection reset by peer")
	}
	n := copy(p, e.data[e.n:])
	e.n += n
	return n, nil
}
func (e *errReader) Close() error { return nil }

// TestFromHTTPPartialBody: a client that hangs up mid-body should surface the
// error, but the bytes that did arrive are still worth having -- for an
// inspector, "the sender disconnected after 5 bytes" is the answer the user
// came for.
func TestFromHTTPPartialBody(t *testing.T) {
	r := httptest.NewRequest("POST", "/webhook", nil)
	r.Body = &errReader{data: []byte("hello")}

	_, err := FromHTTP(r, DefaultMaxBody)
	if err == nil {
		t.Fatal("want an error when the body read fails")
	}

	// And the lower level keeps what it got, which is what the handler will
	// eventually store.
	body, _, err := readBody(&errReader{data: []byte("hello")}, DefaultMaxBody)
	if err == nil {
		t.Fatal("readBody: want error")
	}
	if string(body) != "hello" {
		t.Errorf("readBody kept %q, want the partial %q", body, "hello")
	}
}

// TestReadBodyDoesNotDrain documents the deliberate choice not to consume the
// remainder of an oversized body: whatever is left must still be on the reader.
func TestReadBodyDoesNotDrain(t *testing.T) {
	const limit = 4
	rc := io.NopCloser(strings.NewReader("0123456789"))

	body, truncated, err := readBody(rc, limit)
	if err != nil || !truncated || string(body) != "0123" {
		t.Fatalf("readBody = %q, %v, %v", body, truncated, err)
	}

	// Exactly one byte was consumed beyond the limit: the +1 probe.
	rest, _ := io.ReadAll(rc)
	if string(rest) != "56789" {
		t.Errorf("remaining = %q, want %q (probe reads 1 byte past the limit)", rest, "56789")
	}
}

var _ http.ResponseWriter = (http.ResponseWriter)(nil)
