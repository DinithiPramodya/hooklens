package server

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// clientIP returns the address to rate-limit a request by.
//
// This is the hard part of per-IP anything, and getting it wrong fails in
// one of two directions:
//
//   - Trust r.RemoteAddr blindly, and behind any proxy every request appears
//     to come from the proxy. Per-IP limiting silently becomes GLOBAL
//     limiting, and one client can exhaust everyone's allowance.
//   - Trust X-Forwarded-For blindly, and anyone can forge their identity by
//     setting the header -- so the limit becomes advisory, which is worse
//     than none because it looks like protection.
//
// The only correct answer depends on deployment, so it is configuration:
// trustProxy says whether something in front of us is setting the header.
// Defaulting to false means a misconfiguration under-counts distinct
// clients rather than letting them forge.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if ip, ok := forwardedFor(r); ok {
			return ip
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// No port, or something unparseable. Used as-is rather than
		// discarded: an odd key still limits consistently, where an empty
		// one would lump every such request together.
		return r.RemoteAddr
	}
	return host
}

// forwardedFor reads the FIRST entry of X-Forwarded-For.
//
// First, not last. The header is appended to by each hop, so the leftmost
// value is the original client and the rest are intermediaries. That is
// also the value a client can forge -- which is why this is only consulted
// when the deployment says a trusted proxy is rewriting it.
func forwardedFor(r *http.Request) (string, bool) {
	raw := r.Header.Get("X-Forwarded-For")
	if raw == "" {
		return "", false
	}
	first, _, _ := strings.Cut(raw, ",")
	first = strings.TrimSpace(first)
	if first == "" {
		return "", false
	}

	// Parsed rather than taken as text, so a header full of junk cannot
	// become an unbounded set of distinct limiter keys -- which would be a
	// memory leak driven through the header, reachable by anyone.
	if addr, err := netip.ParseAddr(first); err == nil {
		return addr.Unmap().String(), true
	}
	return "", false
}
