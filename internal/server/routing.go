package server

import (
	"net"
	"strings"
)

// Target says which half of the product a request belongs to.
type Target int

const (
	// TargetApp is the UI and its JSON API -- the thing a human opens.
	TargetApp Target = iota
	// TargetIngest is the capture endpoint -- the thing Stripe talks to.
	TargetIngest
)

// Route is the outcome of resolving a single request.
type Route struct {
	Target Target
	// Slug names the inbox to capture into. Empty when Target is TargetApp.
	Slug string
	// Path is the request path as the capture handler should see it. It differs
	// from the incoming path only for the /e/{slug} form, where the prefix is
	// stripped so a captured request records the path the provider meant.
	Path string
}

// Resolve decides where a request goes, using only its Host header and path.
//
// Three forms are recognised, checked in this order:
//
//	/e/{slug}/anything     (on any host)  -> capture into {slug}, path becomes /anything
//	{slug}.{base}/anything                -> capture into {slug}, path unchanged
//	{base} or www.{base}                  -> the app
//
// Anything else -- a bare IP, a stray CNAME, the platform's internal health
// checker hitting the container directly -- falls through to the app. Capturing
// under an unrecognised host would silently invent inboxes for anyone who
// pointed a DNS record at us.
//
// Resolve is a pure function of its three string arguments. That is deliberate:
// routing is the one place where a mistake means a customer's webhook lands in a
// stranger's inbox, and a pure function is exhaustively testable without an HTTP
// server, a socket, or a database.
func Resolve(base, host, path string) Route {
	// Form 1 is checked first because it must work everywhere, including on the
	// base domain itself, where form 3 would otherwise claim the request.
	if slug, rest, ok := splitPathForm(path); ok {
		return Route{Target: TargetIngest, Slug: slug, Path: rest}
	}

	host = normalizeHost(host)

	// Form 3: the app, on the bare domain or its www alias.
	if host == base || host == "www."+base {
		return Route{Target: TargetApp, Path: path}
	}

	// Form 2: exactly one label in front of the base domain. CutSuffix on
	// "."+base means "a.b.hooklens.dev" yields the sub "a.b", which validSlug
	// rejects for containing a dot -- so nested subdomains do not resolve.
	if sub, ok := strings.CutSuffix(host, "."+base); ok && validSlug(sub) {
		return Route{Target: TargetIngest, Slug: sub, Path: path}
	}

	return Route{Target: TargetApp, Path: path}
}

// pathFormPrefix is the escape hatch that makes every inbox reachable without
// wildcard DNS: hooklens.dev/e/a7f3/webhook is the same inbox as
// a7f3.hooklens.dev/webhook.
//
// It exists for two reasons. Local development has no wildcard DNS, and Windows
// in particular will not resolve *.localhost. And PLAN.md names path routing as
// the documented fallback if wildcard TLS issuance ever fails -- building the
// fallback on day one costs nothing and means the risk is already retired.
const pathFormPrefix = "/e/"

// splitPathForm matches the /e/{slug}[/rest] form and returns the inbox and the
// path the provider actually addressed.
func splitPathForm(path string) (slug, rest string, ok bool) {
	after, found := strings.CutPrefix(path, pathFormPrefix)
	if !found {
		return "", "", false
	}

	slug, rest, hasRest := strings.Cut(after, "/")
	if !validSlug(slug) {
		return "", "", false
	}
	if !hasRest {
		// "/e/a7f3" with no trailing slash: the inbox root.
		return slug, "/", true
	}
	return slug, "/" + rest, true
}

// normalizeHost reduces a Host header to a bare lowercase hostname.
func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))

	// Strip the port. SplitHostPort errors when there is no port -- the common
	// case -- so the original value stands on error. Doing this by hand with
	// LastIndexByte(':') would corrupt an IPv6 literal; SplitHostPort already
	// knows about the brackets.
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	// "hooklens.dev." is the fully qualified form of "hooklens.dev" and is legal
	// in a Host header. Same host, so same route.
	return strings.TrimSuffix(host, ".")
}

// reservedSlugs can never be an inbox. Some are conventional subdomains people
// expect to be able to point elsewhere later (mail, www); the rest are names we
// will plausibly want for ourselves.
//
// This list can only ever grow, and growing it is a breaking change for anyone
// already holding that inbox -- so it is deliberately generous now, while there
// are no users, rather than correct later.
var reservedSlugs = map[string]bool{
	"admin": true, "api": true, "app": true, "assets": true, "blog": true,
	"cdn": true, "dashboard": true, "dev": true, "docs": true, "ftp": true,
	"help": true, "mail": true, "smtp": true, "static": true, "status": true,
	"support": true, "www": true,
}

// validSlug reports whether s is a well-formed inbox name: 3 to 32 characters of
// lowercase letters, digits and hyphens, not starting or ending with a hyphen,
// and not reserved.
//
// This is stricter than DNS permits, on purpose. It is a whitelist, and it is the
// only thing standing between a Host header -- which is entirely attacker
// controlled, arriving before any authentication -- and a database lookup. A
// blacklist here would be a bug waiting for someone to find the character we
// forgot.
func validSlug(s string) bool {
	if len(s) < 3 || len(s) > 32 {
		return false
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	// Byte-wise, not rune-wise: every character we allow is ASCII, so any
	// multi-byte rune fails the switch below and the string is rejected.
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-':
		default:
			return false
		}
	}
	return !reservedSlugs[s]
}
