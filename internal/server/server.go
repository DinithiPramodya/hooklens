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

	// NOTE: none of these are authenticated yet. Anyone who reaches the API can
	// create an inbox and read any inbox whose slug they can guess. Unit 08
	// replaces caller-chosen slugs with generated unguessable ones and adds a
	// capability token. Until then this is a development-only surface.
	mux.HandleFunc("POST /api/endpoints", s.handleCreateEndpoint)
	mux.HandleFunc("GET /api/endpoints/{slug}/requests", s.handleListRequests)
	mux.HandleFunc("GET /api/requests/{id}", s.handleGetRequest)

	return mux
}

func (s *Server) handleCreateEndpoint(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	// A body limit even here: this endpoint is as reachable as any other, and
	// json.Decode on an unbounded reader has the same problem as io.ReadAll.
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	// Validate with the same function the router uses, so a slug that can be
	// created is always a slug that can be routed to. Two separate notions of
	// "valid slug" would drift, and the failure mode is an inbox that exists
	// and cannot receive anything.
	if !validSlug(body.Slug) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "slug must be 3-32 chars of a-z, 0-9 and hyphens, not starting or ending with a hyphen, and not reserved",
		})
		return
	}

	ep, err := s.store.CreateEndpoint(r.Context(), body.Slug, body.Name)
	if errors.Is(err, store.ErrSlugTaken) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "slug already in use"})
		return
	}
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
		"urls": map[string]string{
			"subdomain": "http://" + ep.Slug + "." + s.cfg.BaseDomain + "/",
			"path":      "/e/" + ep.Slug + "/",
		},
	})
}

func (s *Server) handleListRequests(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	ep, err := s.store.EndpointBySlug(r.Context(), slug)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such inbox"})
		return
	}
	if err != nil {
		s.log.Error("lookup endpoint", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "lookup failed"})
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	reqs, err := s.store.ListRequests(r.Context(), ep.ID, limit)
	if err != nil {
		s.log.Error("list requests", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "list failed"})
		return
	}

	out := make([]map[string]any, 0, len(reqs))
	for _, req := range reqs {
		out = append(out, summarise(req))
	}
	writeJSON(w, http.StatusOK, map[string]any{"inbox": ep.Slug, "requests": out})
}

func (s *Server) handleGetRequest(w http.ResponseWriter, r *http.Request) {
	req, err := s.store.GetRequest(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such request"})
		return
	}
	if err != nil {
		// An unparseable id reaches Postgres as a bad uuid cast, which is a
		// client error, not ours. Not worth distinguishing further until the
		// UI exists.
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
