// Package server wires the HTTP surface together: it decides whether a request
// belongs to the app or to the capture endpoint, and hands it to the right one.
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DinithiPramodya/hooklens/internal/broker"
	"github.com/DinithiPramodya/hooklens/internal/config"
	"github.com/DinithiPramodya/hooklens/internal/ingest"
	"github.com/DinithiPramodya/hooklens/internal/metrics"
	"github.com/DinithiPramodya/hooklens/internal/ratelimit"
	"github.com/DinithiPramodya/hooklens/internal/replay"
	"github.com/DinithiPramodya/hooklens/internal/store"
	"github.com/DinithiPramodya/hooklens/internal/tunnel"
	"github.com/DinithiPramodya/hooklens/internal/webui"
)

// Server is the root http.Handler for the whole process.
//
// One binary serves both halves of the product on one port, split by Host
// header. That is a real decision with a real cost -- see DECISIONS.md 01 -- and
// the cost is that it will not survive being run as two replicas, because a
// tunnel held by instance A cannot be reached from instance B.
type Server struct {
	cfg config.Config
	log *slog.Logger
	app http.Handler
	// ingest is the concrete type, not http.Handler: the server owns its
	// configuration (the forward deadline), and hiding it behind the interface
	// would mean reaching it through a type assertion.
	ingest *ingest.Handler
	store  *store.Store
	// broker fans captures out to open streams. Owned here rather than passed
	// in, because its lifetime is exactly this Server's -- it holds no
	// resources to close and nothing outside the process shares it.
	broker *broker.Broker
	// heartbeat is how often an idle stream writes a keepalive comment.
	// A field rather than a constant purely so tests can drive it in
	// milliseconds instead of waiting 20 seconds per assertion.
	heartbeat time.Duration
	handler   http.Handler
	// tunnel serves the CLI's WebSocket. Its lifetime is the process, not a
	// request: see the comment on its baseCtx.
	tunnel *tunnel.Server
	// createLimiter caps inbox creation per IP. That endpoint is
	// deliberately unauthenticated -- there is nobody to authenticate yet
	// -- so a loop against it is free, and this is the only thing standing
	// between that loop and a full database.
	createLimiter *ratelimit.Limiter
	// captureLimiter caps captures per INBOX, not per IP. A provider is a
	// small set of addresses hitting many inboxes, so per-IP here would
	// limit legitimate traffic; per-inbox limits the resource being
	// consumed regardless of who is consuming it.
	captureLimiter *ratelimit.Limiter
	// replayClient refuses private and link-local destinations. Built once:
	// it holds a connection pool, and a per-request client would discard it.
	replayClient *http.Client
	// metrics is the process's published state. Held here rather than in a
	// package global: globals make a counter reachable from anywhere,
	// which means two servers in one test process share it, and they hide
	// which components actually observe what.
	metrics *metrics.App
}

// New builds the root handler.
//
// ctx is the process lifetime -- the signal context from main -- and is
// separate from any request's context. It exists for the tunnel: a WebSocket
// is a hijacked connection, which http.Server.Shutdown neither waits for nor
// knows about, so the only way to tell a tunnel that the process is stopping
// is to cancel a context that outlives every individual request.
func New(ctx context.Context, cfg config.Config, log *slog.Logger, st *store.Store) *Server {
	br := broker.New()
	s := &Server{
		cfg:       cfg,
		log:       log,
		store:     st,
		broker:    br,
		heartbeat: heartbeatInterval,
		metrics:   metrics.NewApp(),
	}

	// The tunnel is built BEFORE ingest, because ingest forwards through its
	// hub. Constructing ingest first and passing nil -- then hoping to set the
	// field afterwards -- is how a handler ends up silently never forwarding.
	s.tunnel = tunnel.New(ctx, log,
		// The store's error is translated at the boundary rather than letting
		// internal/tunnel import internal/store. That keeps the protocol
		// package free of any opinion about how endpoints are persisted.
		func(ctx context.Context, slug, token string) (string, error) {
			ep, err := st.AuthenticateEndpoint(ctx, slug, token)
			switch {
			case errors.Is(err, store.ErrUnauthorized):
				return "", tunnel.ErrUnauthorized
			case err != nil:
				return "", err
			}
			return ep.ID, nil
		},
		func(slug string) string { return "http://" + slug + "." + cfg.BaseDomain + "/" },
		tunnel.Options{},
	)

	// Zero means "use the default", the same contract as ingest.New's
	// maxBody. Not pedantry: a config.Config built by hand -- which every
	// test does -- would otherwise carry a rate of ZERO, and ratelimit.New
	// clamps that to 1/sec. A limiter nobody configured would then reject
	// the second request of every test, which is exactly how this landed
	// the first time.
	rateCreate, rateCapture := cfg.RateCreate, cfg.RateCapture
	if rateCreate <= 0 {
		rateCreate = defaultRateCreate
	}
	if rateCapture <= 0 {
		rateCapture = defaultRateCapture
	}

	// Per minute for creation, per second for captures. Bursts are sized so
	// a normal user never notices: a handful of inboxes in a row, and a
	// provider delivering a backlog.
	s.createLimiter = ratelimit.New(rateCreate/60, 5)
	s.captureLimiter = ratelimit.New(rateCapture, rateCapture*2)

	s.replayClient = replay.SafeClient(replayTimeout)
	s.ingest = ingest.New(log, st, br, s.tunnel.Hub(), cfg.MaxBody)
	s.ingest.SetLimiter(s.captureLimiter)
	s.ingest.SetRecorder(s.metrics)
	s.app = s.appRoutes()

	// The middleware chain is built once, at construction, not per request.
	// Outermost first: withRecover wraps withRequestLog so a panic is still
	// logged as a completed request with a 500, rather than escaping the logger.
	s.handler = withRecover(log, withRequestLog(log, s.metrics, http.HandlerFunc(s.route)))

	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// route is the fork: app or capture.
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	rt := Resolve(s.cfg.BaseDomain, r.Host, r.URL.Path)

	if rt.Target == TargetApp {
		s.app.ServeHTTP(w, r)
		return
	}

	// Clone rather than mutate. r.Clone deep-copies the URL, so rewriting Path
	// on the copy cannot be observed by anything still holding the original --
	// the same reason http.StripPrefix clones. Mutating a request in flight is
	// the kind of bug that only shows up once something else reads it
	// concurrently.
	r2 := r.Clone(ingest.WithSlug(r.Context(), rt.Slug))
	r2.URL.Path = rt.Path
	// RawPath holds the original percent-encoded form. Having rewritten Path we
	// must clear it, or EscapedPath() could return an encoding of the path we
	// just replaced. (net/url does detect the mismatch and re-encode, so this is
	// belt and braces -- but relying on that is relying on an implementation
	// detail of a function whose job is exactly this ambiguity.)
	r2.URL.RawPath = ""
	s.ingest.ServeHTTP(w, r2)
}

// appRoutes is the UI and its API.
//
// Registration order in the source does not matter -- Go's ServeMux picks the
// most SPECIFIC matching pattern, not the first registered. What matters is
// that the specificity ordering below is the one we want:
//
//	GET /api/requests/{id}   most specific: exact path + method
//	/api/                    prefix: any other API path
//	/                        prefix: everything else -> the SPA
func (s *Server) appRoutes() http.Handler {
	mux := http.NewServeMux()

	// "GET /healthz" is a Go 1.22 routing pattern. The method is part of the
	// pattern, so a POST to /healthz gets 405 from the mux itself.
	mux.HandleFunc("GET /healthz", s.handleHealth)

	// /metrics publishes traffic volumes and error rates, which is an
	// information leak on a public interface. The standard answers are a
	// second listener on a private address or an ingress rule; neither is
	// built here, so this is recorded as a deliberate deferral rather than
	// an oversight -- see docs/learn/32-metrics.md.
	mux.Handle("GET /metrics", s.metrics.Handler())

	// Reads require the inbox owner token in an Authorization header (unit 08).
	// Creation is deliberately open: there is nobody to authenticate yet, and an
	// inbox is worthless until its token is held. Rate limiting is Phase 5.
	mux.HandleFunc("POST /api/endpoints", s.handleCreateEndpoint)
	mux.HandleFunc("GET /api/endpoints/{slug}/requests", s.handleListRequests)
	mux.HandleFunc("GET /api/requests/{id}", s.handleGetRequest)
	// POST because it carries a secret, which must never reach a query
	// string -- see the handler.
	mux.HandleFunc("POST /api/requests/{id}/verify", s.handleVerify)
	mux.HandleFunc("POST /api/requests/{id}/replay", s.handleReplay)
	// GET, unlike the two above: no secret, and a diff is worth linking to.
	mux.HandleFunc("GET /api/requests/{id}/diff", s.handleDiff)
	mux.HandleFunc("GET /api/endpoints/{slug}/stream", s.handleStream)

	// The tunnel. One connection per CLI, authenticated by its first frame
	// rather than by a header -- see docs/learn/18-websockets.md.
	mux.HandleFunc("GET /api/tunnel", s.tunnel.Handle)

	// Catch-all for the API namespace, and it is not optional.
	//
	// Without it, GET /api/typo falls through to the SPA handler below, which
	// answers 200 with an HTML document -- and the client's fetch() then tries
	// to JSON.parse a web page. The error you get is a syntax error about "<",
	// nowhere near the actual mistake. This is the sharp edge named in
	// docs/learn/11-spa-and-go-embed.md, closed deliberately.
	mux.HandleFunc("/api/", s.handleAPINotFound)

	// Everything else is the SPA: the embedded build, with a fallback to
	// index.html for client-side routes. Mounted last in specificity, so it
	// only ever sees paths nothing above claimed.
	mux.Handle("/", webui.Handler())

	return mux
}

func (s *Server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]string{
		"error": "no such API endpoint",
		"path":  r.URL.Path,
	})
}

// bearerToken extracts the owner token from the Authorization header.
//
// Header only, never a query parameter. A token in a query string lands in the
// server access log, the proxy log, the browser's history and the Referer
// header of any outbound link -- which is precisely the leak that makes people
// distrust capability URLs in the first place.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// unauthorized writes the single response used for every authentication
// failure, whatever the real cause.
//
// A missing inbox, a wrong token and a malformed header all produce this exact
// body. Distinguishing them would tell an attacker which of 2^128 slugs exist,
// turning a guessing problem into a lookup.
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="hooklens"`)
	writeJSON(w, http.StatusUnauthorized, map[string]string{
		"error": "missing or invalid token",
	})
}

func (s *Server) handleCreateEndpoint(w http.ResponseWriter, r *http.Request) {
	// Checked FIRST, before reading the body or touching the database. A
	// limiter that runs after the expensive work has not saved anything --
	// rejecting in microseconds is the entire economic argument for having
	// one.
	if ok, retry := s.createLimiter.Allow(clientIP(r, s.cfg.TrustProxy)); !ok {
		tooManyRequests(w, retry)
		return
	}

	var body struct {
		Name string `json:"name"`
	}
	// A body limit even here: this endpoint is as reachable as any other, and
	// json.Decode on an unbounded reader has the same problem as io.ReadAll.
	// EOF is fine -- an empty body means an unnamed inbox.
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	// The caller no longer chooses the slug. A caller-chosen slug is by
	// definition guessable -- someone would have taken "stripe" and everyone
	// could have found it. See docs/learn/08-capability-urls.md.
	ep, err := s.store.CreateEndpoint(r.Context(), body.Name)
	if err != nil {
		s.log.Error("create endpoint", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not create inbox"})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         ep.ID,
		"slug":       ep.Slug,
		"name":       ep.Name,
		"created_at": ep.CreatedAt,
		// The only time this value ever leaves the server. It is not stored --
		// only its SHA-256 is -- so it cannot be shown again or recovered.
		"token":         ep.Token,
		"token_warning": "shown once; it is stored only as a hash and cannot be recovered",
		"urls": map[string]string{
			"subdomain": "http://" + ep.Slug + "." + s.cfg.BaseDomain + "/",
			"path":      "/e/" + ep.Slug + "/",
		},
	})
}

func (s *Server) handleListRequests(w http.ResponseWriter, r *http.Request) {
	ep, err := s.store.AuthenticateEndpoint(r.Context(), r.PathValue("slug"), bearerToken(r))
	if errors.Is(err, store.ErrUnauthorized) {
		unauthorized(w)
		return
	}
	if err != nil {
		s.log.Error("authenticate endpoint", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "lookup failed"})
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	// An absent cursor means the first page. A PRESENT but malformed one is a
	// client error, not something to silently treat as "start over" -- that
	// would make a truncated cursor look like an infinite list.
	var after *store.Cursor
	if raw := r.URL.Query().Get("after"); raw != "" {
		c, err := store.ParseCursor(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed cursor"})
			return
		}
		after = &c
	}
	page, err := s.store.ListRequests(r.Context(), ep.ID, limit, after)
	if err != nil {
		s.log.Error("list requests", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "list failed"})
		return
	}

	out := make([]map[string]any, 0, len(page.Requests))
	for _, req := range page.Requests {
		out = append(out, summarise(req))
	}
	body := map[string]any{"inbox": ep.Slug, "requests": out}
	if page.NextCursor != "" {
		body["next_cursor"] = page.NextCursor
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleGetRequest(w http.ResponseWriter, r *http.Request) {
	req, err := s.store.AuthenticateRequest(r.Context(), r.PathValue("id"), bearerToken(r))
	if errors.Is(err, store.ErrUnauthorized) {
		unauthorized(w)
		return
	}
	if err != nil {
		// A malformed id reaches Postgres as a bad uuid cast. Report it as a
		// client error, not a 500 -- and deliberately do not distinguish it
		// from "exists but not yours".
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request id"})
		return
	}

	full := summarise(*req)
	full["headers"] = req.Headers
	// Base64 because the body is bytea: arbitrary bytes, frequently not valid
	// UTF-8, and JSON strings must be. The UI decodes and decides how to render.
	full["body_base64"] = base64.StdEncoding.EncodeToString(req.Body)
	writeJSON(w, http.StatusOK, full)
}

func summarise(r store.StoredRequest) map[string]any {
	m := map[string]any{
		"id":             r.ID,
		"method":         r.Method,
		"path":           r.Path,
		"query":          r.Query,
		"body_size":      r.BodySize,
		"body_truncated": r.BodyTruncated,
		"header_count":   len(r.Headers),
		"received_at":    r.ReceivedAt,
	}
	if r.DeclaredSize != nil {
		m["declared_size"] = *r.DeclaredSize
	}
	if r.SourceIP != nil {
		m["source_ip"] = r.SourceIP.String()
	}
	// The forwarding outcome, omitted entirely when forwarding was never
	// attempted. Absent and null mean different things to a client, and
	// "no tunnel was connected" is better expressed by the field not being
	// there than by a zero that looks like a real status.
	if r.ForwardStatus != nil {
		m["forward_status"] = *r.ForwardStatus
	}
	if r.ForwardError != nil {
		m["forward_error"] = *r.ForwardError
	}
	if r.ForwardMS != nil {
		m["forward_ms"] = *r.ForwardMS
	}
	return m
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Liveness only: this says the process is up and serving. It deliberately
	// does not check Postgres. A health check that fails when a dependency is
	// down invites the platform to kill and restart a process that is working
	// fine, which turns a degraded service into an outage. Dependency checks get
	// their own endpoint in Phase 5.
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// tooManyRequests writes a 429 with a Retry-After.
//
// The header is not decoration. A 429 with no indication of when to try
// again leaves a well-behaved client guessing, and guessing means retrying
// too soon -- so the limiter generates exactly the load it exists to
// prevent.
func tooManyRequests(w http.ResponseWriter, retryAfter time.Duration) {
	// Seconds, rounded UP: rounding down tells a client to retry before a
	// token exists, which produces a second 429 and a small retry storm.
	secs := int(retryAfter.Seconds())
	if retryAfter > 0 && float64(secs) < retryAfter.Seconds() {
		secs++
	}
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error":       "rate limit exceeded",
		"retry_after": secs,
	})
}

// EvictLimiters drops rate-limiter buckets that have been idle.
//
// Called by the sweeper, which already wakes on a timer -- adding a second
// goroutine for a map cleanup would be two things to shut down instead of
// one.
//
// The idle window is generous compared to the refill rate: any bucket
// untouched for this long is certainly full, and a full bucket is
// indistinguishable from a new one, so forgetting it changes nothing.
func (s *Server) EvictLimiters() (create, capture int) {
	const idle = 15 * time.Minute
	return s.createLimiter.Evict(idle), s.captureLimiter.Evict(idle)
}

// EvictTouched drops the ingest handler's last-seen bookkeeping for inboxes
// that have gone quiet.
//
// Same sweeper tick, same reasoning as EvictLimiters, and the same
// safety: forgetting an entry costs one extra UPDATE on the inbox's next
// capture and nothing else.
func (s *Server) EvictTouched() int {
	const idle = 15 * time.Minute
	return s.ingest.EvictTouched(idle)
}

// EvictEndpointCache drops expired inbox-resolution entries.
//
// Entries already expire on read, so this only reaches the ones nobody asked
// for again -- an inbox that went quiet, or a slug someone guessed once.
func (s *Server) EvictEndpointCache() int { return s.ingest.EvictEndpointCache() }

// Rate-limit defaults, used when the config carries zero.
//
// These match the defaults in internal/config; duplicated deliberately so
// that a Server built with a hand-made Config -- which every test does --
// behaves like a real one rather than being accidentally throttled to
// 1/sec by ratelimit.New's own clamping.
const (
	// Inbox creations per IP per minute. Generous for a human, tight for a
	// loop: the endpoint is unauthenticated by design.
	defaultRateCreate = 10
	// Captures per inbox per second. Well above any real provider's
	// delivery rate for a single endpoint, so the limit should be invisible
	// until something is genuinely wrong.
	defaultRateCapture = 50
)

// Metrics exposes the registry so other components can record into it.
func (s *Server) Metrics() *metrics.App { return s.metrics }
