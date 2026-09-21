package replay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// TestBlockedAddresses is the SSRF allow/deny list, stated explicitly.
//
// Each blocked entry is something the SERVER can reach and the requester
// generally cannot -- which is the definition of a confused deputy.
func TestBlockedAddresses(t *testing.T) {
	mustBlock := []struct{ addr, why string }{
		{"127.0.0.1", "loopback"},
		{"127.0.0.53", "loopback, and a real resolver on many Linux boxes"},
		{"::1", "IPv6 loopback"},
		// The bypass this whole function unmaps for. Every Is* predicate
		// returns false for the mapped form.
		{"::ffff:127.0.0.1", "IPv4-mapped loopback"},
		{"169.254.169.254", "AWS/GCP/Azure metadata -- the prize"},
		{"169.254.1.1", "link-local"},
		{"fe80::1", "IPv6 link-local"},
		{"10.0.0.1", "private"},
		{"172.16.0.1", "private"},
		{"172.31.255.255", "private, top of the 172.16/12 range"},
		{"192.168.1.1", "private"},
		{"fc00::1", "IPv6 unique-local"},
		{"::ffff:10.0.0.1", "IPv4-mapped private"},
		{"0.0.0.0", "unspecified"},
		{"::", "IPv6 unspecified"},
		{"224.0.0.1", "multicast"},
		{"100.64.0.1", "carrier-grade NAT"},
	}
	for _, tc := range mustBlock {
		addr := netip.MustParseAddr(tc.addr)
		if _, bad := blocked(addr); !bad {
			t.Errorf("%s (%s) was NOT blocked", tc.addr, tc.why)
		}
	}

	// And the internet must still work, or the feature is pointless.
	mustAllow := []string{
		"1.1.1.1", "8.8.8.8", "93.184.216.34", "2606:4700:4700::1111",
		// 172.15 and 172.32 sit either side of the private 172.16/12 block,
		// which is the range people most often get wrong by hand.
		"172.15.0.1", "172.32.0.1",
	}
	for _, s := range mustAllow {
		if reason, bad := blocked(netip.MustParseAddr(s)); bad {
			t.Errorf("%s was blocked (%s); public addresses must work", s, reason)
		}
	}
}

func TestCheckURL(t *testing.T) {
	for _, tc := range []struct{ name, url, wantIn string }{
		{"no scheme", "example.com", "must be http or https"},
		{"file", "file:///etc/passwd", "must be http or https"},
		{"gopher", "gopher://x/", "must be http or https"},
		{"no host", "http://", "no host"},
		{"empty", "", "no target URL"},
		{"loopback literal", "http://127.0.0.1:8080/x", "loopback"},
		{"metadata literal", "http://169.254.169.254/latest/meta-data/", "link-local"},
		{"private literal", "http://10.1.2.3/", "private"},
		{"mapped loopback", "http://[::ffff:127.0.0.1]/", "loopback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckURL(tc.url)
			if err == nil {
				t.Fatalf("CheckURL(%q) allowed it", tc.url)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error = %q, want %q in it", err, tc.wantIn)
			}
		})
	}

	// A public URL passes the pre-check.
	if err := CheckURL("https://example.com/hook"); err != nil {
		t.Errorf("a public URL was rejected: %v", err)
	}
	// A HOSTNAME is deliberately NOT resolved here -- doing so and trusting
	// the answer later is the DNS-rebinding hole. It passes the pre-check
	// and is caught at the dial.
	if err := CheckURL("http://localhost:8080/"); err != nil {
		t.Errorf("a hostname should pass the pre-check and be caught at the dial, got %v", err)
	}
}

// TestSafeClientBlocksAtTheDial is the load-bearing test. CheckURL is a
// convenience; THIS is the control that cannot be raced, because it runs
// with the address the kernel is about to connect to.
func TestSafeClientBlocksAtTheDial(t *testing.T) {
	// A real loopback server, reached by the hostname `localhost` so the
	// literal-IP pre-check cannot be what stops it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the request reached a loopback server; the dial guard did not fire")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	// httptest gives 127.0.0.1:port; swap in the hostname.
	target := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	if err := CheckURL(target); err != nil {
		t.Fatalf("precondition: the hostname form should pass the pre-check, got %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := ToURL(ctx, SafeClient(5*time.Second), target, Request{Method: "POST", Path: "/x"})
	if err == nil {
		t.Fatal("a loopback destination was reached")
	}

	var blockErr *ErrBlockedDestination
	if !errors.As(err, &blockErr) {
		t.Fatalf("error = %v, want an ErrBlockedDestination", err)
	}
	if !strings.Contains(blockErr.Reason, "loopback") {
		t.Errorf("reason = %q, want it to name loopback", blockErr.Reason)
	}
}

// TestSafeClientDoesNotFollowRedirects: a public URL can 302 to
// 169.254.169.254. The dial guard would catch that too, but refusing is
// simpler than relying on a guard for a path nobody exercises -- and for a
// replay, seeing the 302 is information rather than an obstacle.
func TestSafeClientDoesNotFollowRedirects(t *testing.T) {
	var followed bool
	mux := http.NewServeMux()
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, r *http.Request) { followed = true })
	mux.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Bypass the dial guard for this test by using a client without it: the
	// point here is CheckRedirect, and httptest is always loopback.
	client := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: SafeClient(0).CheckRedirect,
	}
	resp, err := ToURL(t.Context(), client, srv.URL, Request{Method: "POST", Path: "/hook"})
	if err != nil {
		t.Fatalf("ToURL: %v", err)
	}
	if resp.Status != http.StatusFound {
		t.Errorf("status = %d, want the 302 relayed", resp.Status)
	}
	if followed {
		t.Error("the redirect was followed")
	}
}
