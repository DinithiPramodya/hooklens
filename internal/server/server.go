// Package server wires the HTTP surface together: it decides whether a request
// belongs to the app or to the capture endpoint, and hands it to the right one.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/DinithiPramodya/hooklens/internal/config"
	"github.com/DinithiPramodya/hooklens/internal/ingest"
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
	handler http.Handler
}

func New(cfg config.Config, log *slog.Logger) *Server {
	s := &Server{
		cfg:    cfg,
		log:    log,
		ingest: ingest.New(log),
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

	return mux
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
