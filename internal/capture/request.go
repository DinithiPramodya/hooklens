// Package capture turns an incoming *http.Request into a faithful, bounded,
// storable record of what arrived.
//
// It is the most hostile-input-facing code in the project: any method, any path,
// any body, from anyone who learns the URL. Nothing here trusts the sender.
package capture

import (
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"time"
)

// DefaultMaxBody is the ceiling on how much of a body we keep.
//
// 1 MiB is comfortably above real webhook payloads -- Stripe events run a few
// kilobytes, the largest GitHub push events tens of kilobytes -- and small
// enough that a few hundred concurrent captures cannot exhaust memory.
const DefaultMaxBody int64 = 1 << 20

// Header is one header name/value pair.
//
// A slice of these rather than a map, because a map cannot hold two headers with
// the same name and HTTP allows exactly that. See the note in Order below for
// what this does and does not preserve.
type Header struct {
	// Tagged because this struct goes out over the API as-is. Untagged it
	// would serialise as {"Name","Value"} -- the only capitalised keys in an
	// otherwise snake_case API. The stored encoding is unaffected: the store
	// marshals through its own headerJSON DTO precisely so the wire format and
	// the storage format can move independently.
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Request is everything we keep about one captured request.
type Request struct {
	Method string
	Path   string
	Query  string

	// Headers is deterministically ordered -- see headersOf. It is NOT wire
	// order, which net/http does not expose.
	Headers []Header

	// Body is the raw bytes, exactly as received, never parsed and
	// re-serialised. Phase 4 computes signatures over these.
	Body []byte

	// Truncated reports that the sender sent more than we kept.
	Truncated bool

	// DeclaredSize is the sender's Content-Length, or -1 when absent (chunked
	// encoding omits it). It is a CLAIM, not a measurement: compare it against
	// len(Body) rather than trusting it.
	DeclaredSize int64

	// SourceIP is the peer address of the TCP connection. Behind a proxy this
	// is the proxy, not the client -- see the note in sourceIP.
	SourceIP netip.Addr

	ReceivedAt time.Time
}

// ContentType returns the Content-Type header, or "" if absent.
func (req *Request) ContentType() string {
	for _, h := range req.Headers {
		if h.Name == "Content-Type" {
			return h.Value
		}
	}
	return ""
}

// FromHTTP reads r and returns what arrived.
//
// It consumes at most maxBody bytes of the body. If the sender sent more, the
// excess is discarded and Truncated is set -- the request is still captured,
// because for an inspector "something too big arrived" is information, not an
// error to swallow.
func FromHTTP(r *http.Request, maxBody int64) (*Request, error) {
	if maxBody <= 0 {
		maxBody = DefaultMaxBody
	}

	body, truncated, err := readBody(r.Body, maxBody)
	if err != nil {
		return nil, err
	}

	return &Request{
		Method: r.Method,
		// EscapedPath, not Path. Path is percent-DECODED, so /a%00b would put a
		// NUL byte in the string -- which Postgres rejects in a text column. The
		// escaped form is both storable and closer to the literal wire bytes.
		Path:         r.URL.EscapedPath(),
		Query:        r.URL.RawQuery,
		Headers:      headersOf(r),
		Body:         body,
		Truncated:    truncated,
		DeclaredSize: r.ContentLength,
		SourceIP:     sourceIP(r.RemoteAddr),
		ReceivedAt:   time.Now().UTC(),
	}, nil
}

// readBody reads up to maxBody bytes and reports whether there were more.
func readBody(rc io.ReadCloser, maxBody int64) (body []byte, truncated bool, err error) {
	if rc == nil {
		return nil, false, nil
	}

	// The +1 is the whole trick. LimitReader reports io.EOF at its limit,
	// which is indistinguishable from the sender simply finishing -- so asking
	// for exactly maxBody can never tell you whether you truncated. Asking for
	// one byte more makes the two cases distinguishable: getting maxBody+1
	// bytes proves there was more to come.
	limited := io.LimitReader(rc, maxBody+1)

	body, err = io.ReadAll(limited)
	if err != nil {
		// A read error here is normal, not exceptional: the client hung up
		// mid-body, or a timeout fired. Report it and let the caller decide --
		// a partially received body is still worth recording.
		return body, int64(len(body)) > maxBody, fmt.Errorf("read body: %w", err)
	}

	if int64(len(body)) > maxBody {
		// Deliberately NOT draining the rest. Draining would mean reading an
		// unbounded amount from a sender we have already established is sending
		// more than we allow -- which is the exact denial-of-service we are
		// defending against. The cost is that net/http closes this connection
		// instead of reusing it for keep-alive. Losing connection reuse on
		// oversized requests is a good trade.
		return body[:maxBody], true, nil
	}
	return body, false, nil
}

// headersOf extracts headers in a deterministic order.
//
// Three things are worth knowing about what this can and cannot do.
//
// It preserves MULTI-VALUE: two "X-Custom" lines arrive as two entries, because
// net/http models Header as map[string][]string and keeps repeated values in
// arrival order.
//
// It does NOT preserve wire order or original casing. net/http canonicalises
// names before the handler runs ("x-github-event" becomes "X-Github-Event"),
// merges differently-cased duplicates into one key, and hands over a map, which
// has no order at all. Go exposes no API for the original bytes. Sorting by name
// is therefore not fidelity -- it is the best available substitute, chosen
// because determinism makes diffing (Phase 4) meaningful and makes tests stable.
//
// It RESTORES headers net/http removes. Host is the important one: Go moves it
// to r.Host and deletes it from the map, so a naive range over r.Header loses
// the single header that determined which inbox this request landed in.
func headersOf(r *http.Request) []Header {
	// Copy rather than mutate r.Header -- the request is not ours to modify,
	// and middleware upstream may still hold a reference.
	names := slices.Sorted(maps.Keys(r.Header))

	out := make([]Header, 0, len(r.Header)+2)
	for _, name := range names {
		for _, v := range r.Header[name] {
			out = append(out, Header{Name: name, Value: v})
		}
	}

	if r.Host != "" {
		out = append(out, Header{Name: "Host", Value: r.Host})
	}
	// Also stripped from the map by net/http, and worth keeping: its presence
	// is why DeclaredSize is -1.
	for _, te := range r.TransferEncoding {
		out = append(out, Header{Name: "Transfer-Encoding", Value: te})
	}

	// Re-sort so the restored headers land in the same deterministic order as
	// the rest, rather than always at the end.
	slices.SortStableFunc(out, func(a, b Header) int {
		if a.Name != b.Name {
			return cmpString(a.Name, b.Name)
		}
		return 0 // stable: equal names keep arrival order
	})
	return out
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// sourceIP parses the peer address of the connection.
//
// This is deliberately r.RemoteAddr and NOT X-Forwarded-For. XFF is a header,
// which means the client writes it, which means an unauthenticated caller can
// put anything in it. Trusting it without knowing you are behind a proxy that
// overwrites it is how IP-based rate limits get bypassed. When hooklens is
// deployed behind a load balancer this needs revisiting with an explicit list of
// trusted proxy addresses -- logged for Phase 5, not guessed at now.
func sourceIP(remoteAddr string) netip.Addr {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	// netip.Addr rather than net.IP: it is comparable, usable as a map key,
	// and does not allocate. net.IP is a []byte slice with none of those
	// properties.
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{} // zero value; IsValid() reports false
	}
	return addr.Unmap()
}
