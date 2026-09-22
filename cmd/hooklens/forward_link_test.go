package main

import (
	"net/url"
	"strings"
	"testing"
)

// TestInspectLinkOpensThisInbox: the link must carry the tunnel's own inbox,
// in the fragment. A bare server URL opened whatever inbox the browser
// remembered, so forwarded webhooks were invisible in the UI.
func TestInspectLinkOpensThisInbox(t *testing.T) {
	link := inspectLink("http://localhost:8080/", savedInbox{Slug: "abc123def", Token: "tok_-x=y+z/"})

	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parse %q: %v", link, err)
	}
	if u.Scheme+"://"+u.Host+u.Path != "http://localhost:8080/" {
		t.Errorf("base = %q, want the server root with one slash", u.Scheme+"://"+u.Host+u.Path)
	}
	// The token must never be in the query string: that is what lands in
	// access logs and Referer headers.
	if u.RawQuery != "" || strings.Contains(u.RawQuery, "tok") {
		t.Errorf("token leaked into the query string: %q", link)
	}

	// EscapedFragment, not Fragment: url.Parse hands back Fragment already
	// decoded, and decoding it again turns an encoded "+" into a space. A
	// browser gives the page the RAW fragment (location.hash), which is what
	// web/src/lib/handoff.ts parses -- so this reads it the same way.
	frag, err := url.ParseQuery(u.EscapedFragment())
	if err != nil {
		t.Fatalf("fragment %q: %v", u.EscapedFragment(), err)
	}
	if frag.Get("slug") != "abc123def" || frag.Get("token") != "tok_-x=y+z/" {
		t.Errorf("fragment decodes to slug=%q token=%q", frag.Get("slug"), frag.Get("token"))
	}
}
