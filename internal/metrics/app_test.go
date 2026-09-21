package metrics

import (
	"strings"
	"testing"
)

// TestKnownSeriesExistBeforeFirstUse.
//
// A metric with no observations emits NO LINE, so a dashboard shows
// "no data" -- indistinguishable from the exporter being down -- and an
// alert on `hooklens_tunnels_connected == 0` never fires, because the
// series does not exist to be zero. Found by scraping a freshly started
// server and noticing the gauge was simply absent.
func TestKnownSeriesExistBeforeFirstUse(t *testing.T) {
	var b strings.Builder
	NewApp().Registry.Write(&b)
	out := b.String()

	for _, want := range []string{
		"hooklens_tunnels_connected 0",
		"hooklens_tunnel_connections_total 0",
		"hooklens_capture_bytes_total 0",
		`hooklens_captures_total{outcome="stored"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("a fresh registry is missing %q\n---\n%s", want, out)
		}
	}
}

func TestStatusClass(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want string
	}{
		{100, "1xx"}, {200, "2xx"}, {201, "2xx"}, {301, "3xx"},
		{404, "4xx"}, {429, "4xx"}, {500, "5xx"}, {503, "5xx"},
	} {
		if got := StatusClass(tc.in); got != tc.want {
			t.Errorf("StatusClass(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestTunnelGaugeGoesBothWays: the gauge must come back down, or it
// slowly becomes fiction and nobody trusts it. The lifetime counter must
// NOT -- that difference is the whole reason both exist.
func TestTunnelGaugeGoesBothWays(t *testing.T) {
	a := NewApp()

	a.TunnelOpened()
	a.TunnelOpened()
	a.TunnelClosed()

	var b strings.Builder
	a.Registry.Write(&b)
	out := b.String()

	for _, want := range []string{
		"hooklens_tunnels_connected 1",
		"hooklens_tunnel_connections_total 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n---\n%s", want, out)
		}
	}
}
