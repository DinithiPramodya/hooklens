// Package server wires the HTTP surface together: it decides whether a request
// belongs to the app or to the capture endpoint, and hands it to the right one.
package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/DinithiPramodya/hooklens/internal/capture"
	"github.com/DinithiPramodya/hooklens/internal/config"
	"github.com/DinithiPramodya/hooklens/internal/ingest"
	"github.com/DinithiPramodya/hooklens/internal/store"
)

// Server is the root http.Handler for the whole process.
//
// One binary serves both halves of the product on one port, split by Host
// header. That is a real decision with a real cost -- see DECISIONS.md 01 -- and
// the cost is that it will not survive being run as two replicas, because a
// tunnel held by instance A cannot be reached from instance B.
type Server struct {
	cfg     config.Config
	log     *slog.Logger
	app     http.Handler
	ingest  http.Handler
	store   *store.Store
	handler http.Handler
}

func New(cfg config.Config, log *slog.Logger, st *store.Store) *Server {
	s := &Server{
		cfg:    cfg,
		log:    log,
		store:  st,
		ingest: ingest.New(log, st, capture.DefaultMaxBody),
	}
	s.app = s.appRoutes()

	// The middleware chain is built once, at construction, not per request.
	// Outermost first: withRecover wraps withRequestLog so a panic is still
	// logged as a completed request with a 500, rather than escaping the logger.
	s.handler = withRecover(log, withRequestLog(log, http.HandlerFunc(s.route)))

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

// appRoutes is the UI and its API. In Phase 2 the "/" handler is replaced by the
// embedded React build; for now it is a placeholder.
func (s *Server) appRoutes() http.Handler {
	mux := http.NewServeMux()

	// "GET /healthz" and "GET /{$}" are Go 1.22 routing patterns. The method is
	// part of the pattern, so a POST to /healthz gets 405 from the mux itself.
	// "/{$}" means exactly "/" -- without the {$}, "/" is a prefix pattern that
	// matches every unmatched path, which is almost never what you want.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /{$}", s.handleIndex)

	// Reads require the inbox owner token in an Authorization header (unit 08).
	// Creation is deliberately open: there is nobody to authenticate yet, and an
	// inbox is worthless until its token is held. Rate limiting is Phase 5.
	mux.HandleFunc("POST /api/endpoints", s.handleCreateEndpoint)
	mux.HandleFunc("GET /api/endpoints/{slug}/requests", s.handleListRequests)
	mux.HandleFunc("GET /api/requests/{id}", s.handleGetRequest)

	return mux
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

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"name":        "hooklens",
		"base_domain": s.cfg.BaseDomain,
		"note":        "phase 0: the UI lands in phase 2",
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
