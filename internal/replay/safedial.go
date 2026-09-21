// Package replay resends a stored capture, to the tunnel or to a URL.
//
// The URL case makes this server a potential SSRF vector, so most of the
// code here is about destination control rather than about sending. See
// docs/learn/27-replay-and-ssrf.md.
package replay

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// ErrBlockedDestination is returned when a target resolves somewhere we
// refuse to reach.
type ErrBlockedDestination struct {
	Addr   string
	Reason string
}

func (e *ErrBlockedDestination) Error() string {
	return fmt.Sprintf("refusing to connect to %s: %s", e.Addr, e.Reason)
}

// blocked reports whether an address is one this server must not be used to
// reach on a user's behalf.
//
// The list is the standard confused-deputy set. Each entry is something the
// SERVER can reach and the requester generally cannot, which is the whole
// definition of the vulnerability.
func blocked(addr netip.Addr) (string, bool) {
	// Unmap first. An IPv4-mapped IPv6 address like ::ffff:127.0.0.1 is
	// loopback, and every Is* predicate below returns false for it in its
	// mapped form -- which is exactly how this kind of check gets bypassed.
	addr = addr.Unmap()

	switch {
	case addr.IsLoopback():
		return "loopback: this is the hooklens server itself, not your machine", true

	case addr.IsLinkLocalUnicast(), addr.IsLinkLocalMulticast():
		// 169.254.0.0/16 and fe80::/10. On AWS, GCP and Azure the metadata
		// service lives at 169.254.169.254 and hands out credentials to
		// anything that asks it. This is the single most valuable target
		// for an SSRF and the reason this function exists.
		return "link-local: this range includes cloud metadata services", true

	case addr.IsPrivate():
		// 10/8, 172.16/12, 192.168/16, fc00::/7.
		return "private range: reachable from the server but not from the internet", true

	case addr.IsUnspecified():
		return "unspecified address", true

	case addr.IsMulticast(), addr.IsInterfaceLocalMulticast():
		return "multicast", true

	case !addr.IsGlobalUnicast():
		// The broad catch-all, last so the specific reasons above produce
		// better messages.
		return "not a globally routable address", true
	}

	// Ranges the netip predicates do NOT cover, which is the part worth
	// knowing. `IsGlobalUnicast` is about the ADDRESSING architecture, not
	// about whether a packet will route on the public internet -- so
	// carrier-grade NAT space is "global unicast" and still very much
	// somebody's internal network. A test caught this; the first version of
	// this function asserted the opposite in a comment and was wrong.
	for _, p := range extraBlocked {
		if p.Contains(addr) {
			return "reserved range " + p.String(), true
		}
	}
	return "", false
}

// extraBlocked lists ranges that are not private, loopback or link-local by
// the standard predicates, and are still not places to send a user's bytes.
var extraBlocked = []netip.Prefix{
	// RFC 6598 carrier-grade NAT. Global unicast as far as netip is
	// concerned, and on a mobile or ISP network it is the range the
	// subscriber side lives in.
	netip.MustParsePrefix("100.64.0.0/10"),
	// RFC 5737 documentation ranges and RFC 2544 benchmarking. Harmless in
	// practice, blocked because a request to one is always a mistake and
	// saying so is more useful than a timeout.
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	// RFC 6890 "this network".
	netip.MustParsePrefix("0.0.0.0/8"),
	// IPv6 documentation range.
	netip.MustParsePrefix("2001:db8::/32"),
}

// safeTransport is an http.Transport whose Control hook rejects blocked
// addresses at the moment of connection.
//
// At the dial, not before it. Resolving the hostname ourselves and checking
// the result leaves a window in which the name resolves again -- differently
// -- when the connection is actually made. That is DNS rebinding, and a
// check that runs earlier than the connect is a check an attacker can race.
// Control runs with the address the kernel is about to use, which closes it.
func safeTransport() *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
			Control: func(network, address string, _ syscall.RawConn) error {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return fmt.Errorf("unparseable dial address %q", address)
				}
				addr, err := netip.ParseAddr(host)
				if err != nil {
					// Control is called with a literal IP; anything else
					// means an assumption here is wrong, and failing closed
					// is the only safe reading of that.
					return fmt.Errorf("dial address %q is not an IP", host)
				}
				if reason, bad := blocked(addr); bad {
					return &ErrBlockedDestination{Addr: addr.String(), Reason: reason}
				}
				return nil
			},
		}).DialContext,

		// A loopback-free transport has no reason to hold many idle
		// connections; replays are occasional.
		MaxIdleConns:          8,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		// The user's destination, not ours: never inherit a proxy from the
		// server's environment, which could route around the dial check.
		Proxy: nil,
	}
}

// SafeClient is an http.Client that refuses private and link-local
// destinations and does not follow redirects.
//
// Redirects are refused rather than re-checked. Following them would mean
// applying the dial guard to each hop, which works, and would also mean a
// public URL could 302 us somewhere interesting -- so the guard would be
// doing real work on a path nobody tests. Refusing is one line and the
// developer sees the 302, which for a replay is information rather than an
// obstacle.
func SafeClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: safeTransport(),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// CheckURL rejects a target before any connection is attempted.
//
// Belt and braces over the dial guard, and it exists for the error message
// rather than for the security: a scheme or shape we will not accept should
// be reported as such immediately, rather than as a dial failure after a DNS
// lookup. The dial guard remains the thing that is actually load-bearing.
func CheckURL(rawURL string) error {
	u, err := parseHTTPURL(rawURL)
	if err != nil {
		return err
	}

	// If the host is already a literal IP, reject it here with a clear
	// message. A hostname is left to the dial guard, deliberately: resolving
	// it now and trusting the answer later is the rebinding hole.
	if addr, err := netip.ParseAddr(u.Hostname()); err == nil {
		if reason, bad := blocked(addr); bad {
			return &ErrBlockedDestination{Addr: addr.String(), Reason: reason}
		}
	}
	return nil
}
