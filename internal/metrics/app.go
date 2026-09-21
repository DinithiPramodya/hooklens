package metrics

import (
	"net/http"
	"strings"
	"time"
)

// App is every metric hooklens publishes.
//
// A struct of named fields rather than a global registry with package-level
// vars. Globals make a metric reachable from anywhere, which sounds
// convenient and means two tests in one process share counters -- and it
// hides which components actually observe what. Passing this explicitly
// makes the dependency visible in every constructor that takes it.
//
// Every metric here should answer a question somebody asks at 3am. One
// that nobody would alert on is decoration, and decoration costs
// cardinality.
type App struct {
	Registry *Registry

	// Captures is the top-level "is it working" counter, labelled by what
	// happened: stored, rejected, rate_limited.
	Captures *CounterVec
	// CaptureBytes tracks volume, which is the thing that fills a disk.
	CaptureBytes *CounterVec

	// Forwards answers "are webhooks reaching anybody", labelled by the
	// same outcome codes the database stores -- deliberately, so a
	// dashboard and a SQL query agree.
	Forwards *CounterVec
	// ForwardDuration is the round trip to a developer's laptop. A
	// histogram rather than a gauge because a single current value says
	// nothing about the request that took ten seconds.
	ForwardDuration *HistogramVec

	// TunnelsConnected is a GAUGE: it goes down, and the current value is
	// the information. A counter of connections would not answer "is
	// anybody connected right now", which is the actual question.
	TunnelsConnected *GaugeVec
	// TunnelConnections counts lifetime connections, which together with
	// the gauge distinguishes "nobody has connected" from "everybody keeps
	// reconnecting" -- indistinguishable from either metric alone.
	TunnelConnections *CounterVec

	// HTTPRequests is labelled by method and status CLASS, not status.
	// Per-status is 60-odd series per method for no extra insight, and
	// per-PATH would be unbounded -- one series per URL a stranger
	// invents, which is the cardinality mistake that kills a Prometheus.
	HTTPRequests *CounterVec
	// HTTPDuration is the latency histogram, labelled the same way.
	HTTPDuration *HistogramVec
}

// NewApp builds the registry and every metric on it.
func NewApp() *App {
	r := NewRegistry()
	a := &App{
		Registry: r,

		Captures: r.NewCounter("hooklens_captures_total",
			"Webhooks received, by outcome."),
		CaptureBytes: r.NewCounter("hooklens_capture_bytes_total",
			"Bytes of request body stored."),

		Forwards: r.NewCounter("hooklens_forwards_total",
			"Forwarding attempts, by outcome."),
		ForwardDuration: r.NewHistogram("hooklens_forward_duration_seconds",
			"Round trip to the local app, in seconds.", DefaultBuckets),

		TunnelsConnected: r.NewGauge("hooklens_tunnels_connected",
			"Tunnels currently attached."),
		TunnelConnections: r.NewCounter("hooklens_tunnel_connections_total",
			"Tunnel connections accepted."),

		HTTPRequests: r.NewCounter("hooklens_http_requests_total",
			"HTTP requests, by method and status class."),
		HTTPDuration: r.NewHistogram("hooklens_http_duration_seconds",
			"HTTP request duration, in seconds.", DefaultBuckets),
	}

	// Initialise the series we know exist, so they appear in the very first
	// scrape rather than on first use.
	//
	// This matters more than it looks. A metric with no observations emits
	// NO LINE, so a dashboard shows "no data" -- which is indistinguishable
	// from the exporter being down, and an alert on
	// `hooklens_tunnels_connected == 0` never fires because the series does
	// not exist to be zero. Found by scraping a freshly started server and
	// noticing the gauge was simply absent.
	a.TunnelsConnected.Set(nil, 0)
	a.TunnelConnections.Add(nil, 0)
	a.CaptureBytes.Add(nil, 0)
	for _, outcome := range []string{"stored", "rate_limited"} {
		a.Captures.Add(Labels{"outcome": outcome}, 0)
	}

	return a
}

// Handler serves the registry in the Prometheus text format.
//
// Note what is NOT here: authentication. /metrics publishes traffic volumes
// and error rates, which is an information leak on a public interface --
// the standard answer is to bind it to a private interface or put it behind
// the ingress, and doing that here would mean a second listener. Recorded
// as a deliberate deferral rather than an oversight; see the note.
func (a *App) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		a.Registry.Write(&b)

		// The version the Prometheus text parser expects. Without the
		// version parameter some scrapers fall back to guessing.
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	})
}

// StatusClass turns 201 into "2xx".
//
// The cardinality decision, in one function. Labelling by exact status
// gives roughly sixty series per method and answers no question the class
// does not: "are we erroring" is 5xx, "are clients misbehaving" is 4xx. The
// exact code is in the logs, where per-event detail belongs.
func StatusClass(status int) string {
	switch {
	case status < 200:
		return "1xx"
	case status < 300:
		return "2xx"
	case status < 400:
		return "3xx"
	case status < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

// ObserveHTTP records one finished request.
func (a *App) ObserveHTTP(method string, status int, d time.Duration) {
	l := Labels{"method": method, "status": StatusClass(status)}
	a.HTTPRequests.Inc(l)
	a.HTTPDuration.Observe(l, d.Seconds())
}

// ---- the ingest.Recorder surface ----
//
// Implemented on App so internal/ingest can depend on a three-method
// interface it declares itself rather than on this package.

func (a *App) CaptureStored(bytes int) {
	a.Captures.Inc(Labels{"outcome": "stored"})
	a.CaptureBytes.Add(nil, int64(bytes))
}

func (a *App) CaptureRejected(reason string) {
	a.Captures.Inc(Labels{"outcome": reason})
}

func (a *App) Forwarded(outcome string, seconds float64) {
	a.Forwards.Inc(Labels{"outcome": outcome})
	a.ForwardDuration.Observe(Labels{"outcome": outcome}, seconds)
}

// TunnelOpened and TunnelClosed keep the gauge and the lifetime counter in
// step. Together they distinguish "nobody has connected" from "everybody
// keeps reconnecting", which neither answers alone.
func (a *App) TunnelOpened() {
	a.TunnelsConnected.Inc(nil)
	a.TunnelConnections.Inc(nil)
}

func (a *App) TunnelClosed() { a.TunnelsConnected.Dec(nil) }
