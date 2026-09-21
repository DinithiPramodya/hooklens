package diff

import (
	"sort"
	"strings"
)

// Header is one header. Its own type here for the reason every other leaf
// package in this repo declares one.
type Header struct {
	Name  string
	Value string
}

// Headers compares two header lists.
//
// Not by coercing them into objects and reusing the JSON walk, which is the
// tempting shortcut. Headers are an ORDERED LIST WITH DUPLICATES ALLOWED --
// that is the whole reason they are stored as a list rather than a map
// (docs/learn/07-storing-a-request.md) -- and a map would silently collapse
// two Set-Cookie headers into one before the comparison began.
func Headers(left, right []Header) []Change {
	// Grouped by lowercased name, because HTTP header names are
	// case-insensitive and two captures from the same provider can differ
	// in casing for no meaningful reason.
	l := group(left)
	r := group(right)

	names := make([]string, 0, len(l)+len(r))
	seen := make(map[string]bool, len(l)+len(r))
	for n := range l {
		names, seen[n] = append(names, n), true
	}
	for n := range r {
		if !seen[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)

	var out []Change
	for _, name := range names {
		lv, inL := l[name]
		rv, inR := r[name]
		path := "header:" + name

		switch {
		case !inR:
			out = append(out, Change{Path: path, Op: Removed, Old: strings.Join(lv, ", ")})
		case !inL:
			out = append(out, Change{Path: path, Op: Added, New: strings.Join(rv, ", ")})
		default:
			// Compared as a joined list rather than element by element.
			// Order within one name is meaningful, and a per-element diff
			// of two Set-Cookie headers is noise where "these differ" is
			// the useful answer.
			ls, rs := strings.Join(lv, ", "), strings.Join(rv, ", ")
			if ls != rs {
				out = append(out, Change{Path: path, Op: Changed, Old: ls, New: rs})
			}
		}
	}
	return out
}

func group(hs []Header) map[string][]string {
	out := make(map[string][]string, len(hs))
	for _, h := range hs {
		n := strings.ToLower(h.Name)
		out[n] = append(out[n], h.Value)
	}
	return out
}

// VolatileHeaders are the ones that differ on every request by design.
//
// Not filtered automatically -- a diff that silently hides fields is a diff
// that lies. Exported so the caller can offer it as a toggle, which puts
// the decision where it belongs: with the person who knows whether the
// timestamp is the thing they are investigating.
var VolatileHeaders = []string{
	"date",
	"x-request-id",
	"x-hooklens-id",
	"stripe-signature",
	"x-hub-signature",
	"x-hub-signature-256",
	"x-shopify-hmac-sha256",
	"x-slack-signature",
	"x-slack-request-timestamp",
	"x-github-delivery",
	"user-agent",
	"content-length",
}

// WithoutVolatile removes changes on headers that differ every time.
func WithoutVolatile(changes []Change) []Change {
	volatile := make(map[string]bool, len(VolatileHeaders))
	for _, h := range VolatileHeaders {
		volatile["header:"+h] = true
	}

	out := make([]Change, 0, len(changes))
	for _, c := range changes {
		if !volatile[c.Path] {
			out = append(out, c)
		}
	}
	return out
}
