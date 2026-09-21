// Package ingest is the capture endpoint: the handler a webhook provider talks
// to. It accepts any method, any path, any headers and any body, because we do
// not get to say what a provider sends -- our job is to record it faithfully.
package ingest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DinithiPramodya/hooklens/internal/broker"
	"github.com/DinithiPramodya/hooklens/internal/capture"
	"github.com/DinithiPramodya/hooklens/internal/store"
	"github.com/DinithiPramodya/hooklens/internal/tunnel"
)

// ReservedPrefix is the one path namespace this handler will not capture.
//
// Something must answer health checks on an inbox host, because proving that
// https://anything.hooklens.dev serves a valid certificate is exactly how we
// verify wildcard TLS. But a provider is entitled to POST to any path, including
// /healthz -- so instead of stealing a plausible path we reserve a namespace no
// real provider will ever use, and capture literally everything else.
const ReservedPrefix = "/_hooklens/"

// slugKey is the context key carrying the resolved inbox. It is an unexported
// zero-size type so no other package can collide with it -- the standard Go
// idiom, and the reason context keys are never plain strings.
type slugKey struct{}

// WithSlug returns a copy of ctx carrying the inbox this request resolved to.
func WithSlug(ctx context.Context, slug string) context.Context {
	return context.WithValue(ctx, slugKey{}, slug)
}

// SlugFrom returns the inbox carried by ctx, or "" if there is none.
func SlugFrom(ctx context.Context) string {
	slug, _ := ctx.Value(slugKey{}).(string)
	return slug
}

// Handler captures incoming requests.
type Handler struct {
	log     *slog.Logger
	store   *store.Store
	broker  *broker.Broker
	maxBody int64
	// fwd delivers captures to a connected tunnel. Nil is a supported state,
	// not a missing dependency: hooklens is a working inspector with no
	// tunnel feature at all, and that path must stay clean.
	fwd Forwarder
	// forwardTimeout bounds one round trip. A field rather than a constant
	// purely so tests can drive it in milliseconds instead of waiting 30
	// seconds per assertion -- the same reasoning as Server.heartbeat.
	forwardTimeout time.Duration
	// limiter caps captures per inbox. Nil means unlimited, which keeps
	// every test that does not care about limiting free of setup.
	limiter Limiter
	// rec records metrics. Nil is fine, for the same reason as the others.
	rec Recorder

	// touchMu guards touched.
	touchMu sync.Mutex
	// touched remembers when each inbox's last_seen_at was last written, so
	// the write can be skipped for the rest of the interval.
	//
	// This exists because of a load test. Every capture used to
	// `update endpoints set last_seen_at = ...` for its inbox -- and at 1,000
	// req/s to ONE inbox, that is a thousand updates a second to a single
	// row. Postgres takes a row lock per update, so they serialise: the
	// pipeline topped out at 280 req/s while the same database could absorb
	// 2,000 inserts/s, because inserts go to different rows and these all
	// went to the same one. The bottleneck was a column nobody looks at more
	// than once a minute. See docs/learn/33-load-testing.md.
	touched map[string]time.Time

	// endpoints caches slug -> inbox for a few seconds, removing the third
	// of the capture path's three database round trips. See cache.go.
	endpoints *endpointCache
}

// touchInterval is how stale last_seen_at is allowed to get.
//
// The column drives a "last active" display. A minute of staleness is
// invisible there and removes 99.9% of the writes at any interesting rate.
const touchInterval = time.Minute

func New(log *slog.Logger, st *store.Store, br *broker.Broker, fwd Forwarder, maxBody int64) *Handler {
	if maxBody <= 0 {
		maxBody = capture.DefaultMaxBody
	}
	return &Handler{
		log: log, store: st, broker: br, fwd: fwd, maxBody: maxBody,
		forwardTimeout: defaultForwardTimeout,
		touched:        map[string]time.Time{},
		endpoints:      newEndpointCache(),
	}
}

// SetForwardTimeout overrides the round-trip deadline.
//
// Must be called before the handler serves anything: it is a plain field
// write with no lock, which is safe only while nothing is reading it.
func (h *Handler) SetForwardTimeout(d time.Duration) {
	if d > 0 {
		h.forwardTimeout = d
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, ReservedPrefix) {
		h.serveReserved(w, r)
		return
	}
	h.capture(w, r)
}

// serveReserved answers the /_hooklens/ namespace on an inbox host.
func (h *Handler) serveReserved(w http.ResponseWriter, r *http.Request) {
	switch strings.TrimPrefix(r.URL.Path, ReservedPrefix) {
	case "health":
		writeJSON(w, http.StatusOK, map[string]string{
			"status": "ok",
			"inbox":  SlugFrom(r.Context()),
		})
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "unknown reserved path",
		})
	}
}

// capture reads the request and stores it.
func (h *Handler) capture(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := SlugFrom(ctx)

	// Resolve the inbox BEFORE reading the body. An unknown slug means we are
	// about to read up to a megabyte from someone for a destination that does
	// not exist -- which is free storage-exhaustion for anyone scanning
	// subdomains. Rejecting first costs one indexed lookup, or, most of the
	// time, a map read.
	ep, err := h.resolve(ctx, slug)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "no such inbox",
			"inbox": slug,
		})
		return
	}
	if err != nil {
		h.log.Error("endpoint lookup failed", "inbox", slug, "err", err)
		// 503, not 500: this is "come back later", and a provider's retry is
		// exactly the right behaviour here -- unlike the Phase 0 quiz Q1 case,
		// we genuinely have not stored anything.
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily unavailable"})
		return
	}

	// Limited AFTER resolving the inbox -- the endpoint id is the key -- and
	// BEFORE reading the body, so a refused request never costs us up to a
	// megabyte of read.
	//
	// Above the durability line, deliberately. A 429 here means nothing was
	// stored, so a provider retrying produces no duplicate. The same check
	// placed below the line would reject a capture we already hold, which is
	// the Phase 0 quiz Q1 bug wearing a rate limiter.
	if h.limiter != nil {
		if ok, retry := h.limiter.Allow(ep.ID); !ok {
			h.log.Warn("capture rate limited", "inbox", slug, "retry_after", retry)
			w.Header().Set("Retry-After", strconv.Itoa(max(1, int(retry.Seconds()+0.999))))
			if h.rec != nil {
				h.rec.CaptureRejected("rate_limited")
			}
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error": "rate limit exceeded for this inbox",
				"hint":  "Raise HOOKLENS_RATE_CAPTURE if this is legitimate traffic.",
			})
			return
		}
	}

	req, err := capture.FromHTTP(r, h.maxBody)
	if err != nil {
		// A body read failure is the client's doing -- they hung up, or timed
		// out -- not ours. Log it and carry on with whatever did arrive: for an
		// inspector, "the sender disconnected after 5 bytes" is the answer the
		// user came for, not an error to swallow.
		h.log.Warn("partial body", "inbox", slug, "err", err)
	}
	if req == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "could not read request"})
		return
	}

	id, err := h.store.InsertRequest(ctx, ep.ID, req)
	if err != nil {
		h.log.Error("insert failed", "inbox", slug, "err", err)
		// Nothing was stored, so asking the provider to retry is honest.
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily unavailable"})
		return
	}

	if h.rec != nil {
		h.rec.CaptureStored(len(req.Body))
	}

	// ---- the request is durable from here on ----
	//
	// Everything below this line must be incapable of turning a stored request
	// into a non-2xx response. A provider treats any non-2xx as "not delivered"
	// and retries, so a failure here would produce a duplicate for an event we
	// already hold -- the exact bug from Phase 0 quiz Q1.

	// Best-effort, deliberately ignoring the error: last_seen_at is a nicety.
	// Coalesced, because a nicety that costs a serialised row lock per
	// capture is the most expensive thing in this handler -- see shouldTouch.
	if h.shouldTouch(ep.ID, req.ReceivedAt) {
		if err := h.store.TouchEndpoint(ctx, ep.ID, req.ReceivedAt); err != nil {
			h.log.Warn("touch endpoint failed", "inbox", slug, "err", err)
		}
	}

	// Notify anyone watching this inbox. Below the durability line, and safe to
	// be here for two reasons: Publish cannot block (a backgrounded browser tab
	// must never stall a provider's request), and it cannot fail -- there is no
	// error to accidentally turn into a non-2xx.
	//
	// Note this is NOT the request itself. The event carries an id and enough
	// to render a row; a client wanting the body fetches it. Streaming up to a
	// megabyte of body to every open tab on every capture would make one large
	// webhook a fan-out problem.
	h.publish(ep.ID, id, req)

	h.log.Info("captured",
		"id", id,
		"inbox", slug,
		"method", req.Method,
		"path", req.Path,
		"headers", len(req.Headers),
		"body_bytes", len(req.Body),
		"truncated", req.Truncated,
	)

	// Forwarding. Still below the durability line, so nothing here may turn a
	// stored capture into a non-2xx -- forward() returns a decision, never an
	// error.
	fo := h.forward(ctx, ep.ID, req)

	if h.rec != nil && fo.out != (store.ForwardOutcome{}) {
		// The same outcome vocabulary the database stores, deliberately, so
		// a dashboard and a SQL query cannot disagree about what happened.
		outcome := fo.out.Error
		if outcome == "" {
			outcome = "delivered"
		}
		h.rec.Forwarded(outcome, float64(fo.out.Elapsed)/1000)
	}

	if fo.out != (store.ForwardOutcome{}) {
		// Recorded on its own context. ctx may already be cancelled -- the
		// provider hung up during a slow forward -- and writing the outcome is
		// the only durable evidence of what happened.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if err := h.store.RecordForward(rctx, id, fo.out); err != nil {
			h.log.Warn("record forward failed", "inbox", slug, "id", id, "err", err)
		}
		cancel()
	}

	if fo.resp != nil && fo.resp.Error == "" {
		// The developer's app answered. Relay it verbatim -- that is the whole
		// point of the tunnel.
		h.log.Info("forwarded", "id", id, "inbox", slug,
			"status", fo.resp.Status, "ms", fo.out.Elapsed)
		writeForwarded(w, fo.resp, id)
		return
	}

	w.Header().Set("X-Hooklens-Id", id)

	// Either there is no tunnel, or delivery failed. The provider gets a
	// success either way, because the capture IS stored and a non-2xx would
	// make it retry an event we already hold. The header says what happened
	// for anyone looking; the UI reads the recorded code.
	if fo.out.Error != "" {
		h.log.Info("forward failed", "id", id, "inbox", slug,
			"reason", fo.out.Error, "ms", fo.out.Elapsed)
		w.Header().Set("X-Hooklens-Forward", fo.out.Error)
	} else {
		w.Header().Set("X-Hooklens-Forward", "captured")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"captured":   true,
		"id":         id,
		"inbox":      slug,
		"body_bytes": len(req.Body),
		"truncated":  req.Truncated,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// publish announces a capture to anyone streaming this inbox.
//
// Deliberately silent on failure: it sits below the durability line, so the
// only honest response to a problem here is a log line. Returning an error
// would invite a caller to turn it into a non-2xx, which is the bug this
// ordering exists to prevent.
func (h *Handler) publish(endpointID, requestID string, req *capture.Request) {
	// Skip the serialisation entirely when nobody is listening, which is the
	// common case -- most inboxes have no browser attached most of the time.
	if h.broker == nil || h.broker.Subscribers(endpointID) == 0 {
		return
	}

	// A summary, not the request. See the call site for why the body is not
	// included.
	payload, err := json.Marshal(map[string]any{
		"id":             requestID,
		"method":         req.Method,
		"path":           req.Path,
		"query":          req.Query,
		"body_size":      len(req.Body),
		"body_truncated": req.Truncated,
		"header_count":   len(req.Headers),
		"received_at":    req.ReceivedAt,
	})
	if err != nil {
		h.log.Error("marshal capture event", "err", err)
		return
	}

	h.broker.Publish(endpointID, broker.Message{Event: "capture", Data: string(payload)})
}

// Forwarder hands a captured request to a connected tunnel.
//
// An interface declared here, in the consumer, rather than internal/ingest
// importing internal/tunnel's concrete Hub. Same reasoning as AuthFunc in the
// other direction: ingest does not need to know that tunnels are WebSockets,
// and this keeps the dependency one-way and the tests free of sockets.
type Forwarder interface {
	Forward(ctx context.Context, endpointID string, req tunnel.Request) (*tunnel.Response, error)
}

// defaultForwardTimeout bounds one round trip to the developer's machine.
//
// Chosen to sit under the shortest common provider timeout (Stripe and GitHub
// both give around 10s before they call a delivery failed, but several give
// 30s), so in the normal case WE give up first and answer deliberately rather
// than having the provider hang up on us mid-forward and retry.
const defaultForwardTimeout = 30 * time.Second

// forwardOutcome is the decision made about one attempt: what to tell the
// provider, and what to record on the capture.
type forwardOutcome struct {
	resp *tunnel.Response
	out  store.ForwardOutcome
}

// forward attempts delivery and reports what happened.
//
// It is BELOW the durability line and therefore cannot fail in a way the
// caller could turn into a non-2xx. Every error path here becomes a recorded
// code, never a returned error.
func (h *Handler) forward(ctx context.Context, endpointID string, req *capture.Request) forwardOutcome {
	if h.fwd == nil {
		return forwardOutcome{}
	}

	// A truncated capture is NOT forwarded.
	//
	// The obvious behaviour -- send what we kept -- delivers a corrupt payload
	// that looks complete. A JSON parser then fails in a way that blames the
	// sender, and a signature verifier fails in a way that blames the secret;
	// neither points at truncation, because nothing says truncation happened.
	// Refusing is worse for exactly one case (a handler that tolerates partial
	// bodies) and better for every other.
	//
	// Nothing is lost for inspection: the capture is stored, visible in the
	// UI, and flagged. The escape hatch is raising HOOKLENS_MAX_BODY.
	if req.Truncated {
		return forwardOutcome{out: store.ForwardOutcome{Error: "too_large"}}
	}

	// A fresh context, deliberately NOT derived from the request's.
	//
	// r.Context() is cancelled the moment the provider hangs up -- and a
	// provider whose own timeout is shorter than ours does exactly that. If
	// the forward were tied to it, the developer's app would have its request
	// cancelled mid-handler, which looks to them like their own code
	// misbehaving. Finishing the delivery and recording the outcome is the
	// honest thing; nobody is waiting for the response, but the capture's
	// record of what happened is still worth having.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.forwardTimeout)
	defer cancel()

	start := time.Now()
	resp, err := h.fwd.Forward(fctx, endpointID, tunnel.Request{
		Method:  req.Method,
		Path:    req.Path,
		Query:   req.Query,
		Headers: toTunnelHeaders(req.Headers),
		BodyB64: base64.StdEncoding.EncodeToString(req.Body),
	})
	elapsed := int(time.Since(start).Milliseconds())

	if err != nil {
		return forwardOutcome{out: store.ForwardOutcome{
			Error:   forwardErrorCode(err),
			Elapsed: elapsed,
		}}
	}

	// The CLI reached us but could not reach the local app. Distinct from a
	// 5xx: the app was never asked. Reporting this as an application error
	// would tell a developer their handler is broken when it is not running.
	if resp.Error != "" {
		return forwardOutcome{resp: resp, out: store.ForwardOutcome{
			Error:   "unreachable",
			Elapsed: elapsed,
		}}
	}

	return forwardOutcome{resp: resp, out: store.ForwardOutcome{
		Status:  resp.Status,
		Elapsed: elapsed,
	}}
}

// forwardErrorCode maps a hub error to the short code stored in the database.
//
// The CHECK constraint in migration 00006 lists the permitted values, so an
// unmapped error must fall back to one of them rather than to the error's own
// text -- otherwise a new error type anywhere upstream turns into a constraint
// violation on the UPDATE, which would be logged as a database fault far from
// its actual cause.
func forwardErrorCode(err error) string {
	switch {
	case errors.Is(err, tunnel.ErrNoTunnel):
		return "no_tunnel"
	case errors.Is(err, tunnel.ErrTimeout):
		return "timeout"
	case errors.Is(err, tunnel.ErrDisconnected):
		return "disconnected"
	case errors.Is(err, tunnel.ErrOverloaded):
		return "overloaded"
	default:
		return "protocol"
	}
}

func toTunnelHeaders(hs []capture.Header) []tunnel.Header {
	out := make([]tunnel.Header, len(hs))
	for i, h := range hs {
		out[i] = tunnel.Header{Name: h.Name, Value: h.Value}
	}
	return out
}

// writeForwarded relays the local app's response to the provider.
//
// Status, headers and body come from the developer's application, which is the
// entire point: they must be able to test what their handler actually returns.
func writeForwarded(w http.ResponseWriter, resp *tunnel.Response, captureID string) {
	body, err := base64.StdEncoding.DecodeString(resp.BodyB64)
	if err != nil {
		// A malformed body from our own CLI. Relay the status anyway rather
		// than inventing a failure: the status is the part a provider acts on.
		body = nil
	}
	for _, hdr := range resp.Headers {
		// Hop-by-hop headers describe the CLI's connection to the local app,
		// not ours to the provider. Relaying them would advertise a transfer
		// encoding or a keep-alive that does not apply to this response.
		if isHopByHop(hdr.Name) {
			continue
		}
		w.Header().Add(hdr.Name, hdr.Value)
	}
	w.Header().Set("X-Hooklens-Forward", "delivered")
	// The capture id, because on a successful forward the BODY belongs to the
	// developer's app -- so this header is the only way anything downstream can
	// correlate the response it received with the capture we stored.
	w.Header().Set("X-Hooklens-Id", captureID)
	status := resp.Status
	if status < 100 || status > 599 {
		// A CLI that sent nonsense must not make us panic in WriteHeader.
		status = http.StatusBadGateway
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// isHopByHop reports whether a header applies to a single transport hop and
// must not be forwarded. RFC 9110 section 7.6.1, plus Content-Length, which
// net/http computes for the response it is actually writing.
func isHopByHop(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade", "content-length":
		return true
	}
	return false
}

// SetMaxBody overrides the capture body cap.
//
// Same contract as SetForwardTimeout: called before the handler serves
// anything, because it is a plain field write with no lock.
func (h *Handler) SetMaxBody(n int64) {
	if n > 0 {
		h.maxBody = n
	}
}

// Limiter caps how often an inbox may be written to.
//
// An interface declared here, in the consumer, like Forwarder above.
// internal/ingest does not need to know how the limiting is implemented,
// and a nil limiter means no limiting.
type Limiter interface {
	Allow(key string) (ok bool, retryAfter time.Duration)
}

// SetLimiter installs a per-inbox rate limiter.
//
// Same contract as the other setters: called before the handler serves
// anything, because it is a plain field write with no lock.
func (h *Handler) SetLimiter(l Limiter) { h.limiter = l }

// Recorder is the metrics surface ingest needs.
//
// An interface declared here, like Forwarder and Limiter. internal/ingest
// records three things and should not import a package that knows about
// histograms and exposition formats to do it.
type Recorder interface {
	CaptureStored(bytes int)
	CaptureRejected(reason string)
	Forwarded(outcome string, seconds float64)
}

// SetRecorder installs a metrics recorder. Before serving, no lock.
func (h *Handler) SetRecorder(r Recorder) { h.rec = r }

// shouldTouch reports whether this inbox's last_seen_at is due for a write,
// and records the decision.
//
// The whole point is to keep at most one UPDATE per inbox per touchInterval
// instead of one per capture. The map is keyed by endpoint id, so inboxes do
// not contend with each other; only the mutex is shared, and it is held for
// a map lookup rather than a database round trip.
//
// It marks the inbox as touched BEFORE the write happens, not after. That is
// deliberate: if the update fails, the next capture within the interval will
// not retry it, and that is correct -- last_seen_at is a nicety, and retrying
// a failing write once per capture is how a nicety becomes an outage. The
// next interval tries again.
//
// Per-process, so N instances write at most N times per interval. That is
// fine at any N worth deploying, and the alternative -- a shared counter --
// would mean a round trip to coordinate avoiding a round trip.
func (h *Handler) shouldTouch(endpointID string, now time.Time) bool {
	h.touchMu.Lock()
	defer h.touchMu.Unlock()

	if last, ok := h.touched[endpointID]; ok && now.Sub(last) < touchInterval {
		return false
	}
	h.touched[endpointID] = now
	return true
}

// EvictTouched forgets inboxes not seen for idle, and returns how many.
//
// Without this the map is a slow leak: one entry per inbox that ever received
// a capture, for the life of the process. Forgetting an entry is always safe
// -- it costs one extra UPDATE on the next capture, which is exactly what
// would have happened anyway once the interval elapsed.
//
// Called by the sweeper on its existing tick, for the same reason
// EvictLimiters is: a second goroutine for a map cleanup would be a second
// thing to shut down.
func (h *Handler) EvictTouched(idle time.Duration) int {
	h.touchMu.Lock()
	defer h.touchMu.Unlock()

	cutoff := time.Now().Add(-idle)
	n := 0
	for id, at := range h.touched {
		if at.Before(cutoff) {
			delete(h.touched, id)
			n++
		}
	}
	return n
}
