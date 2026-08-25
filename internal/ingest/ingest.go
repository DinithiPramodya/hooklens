// Package ingest is the capture endpoint: the handler a webhook provider talks
// to. It accepts any method, any path, any headers and any body, because we do
// not get to say what a provider sends -- our job is to record it faithfully.
package ingest

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
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
	log *slog.Logger
}

func New(log *slog.Logger) *Handler {
	return &Handler{log: log}
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

// capture is a Phase 0 stub. It proves routing reaches the right place and
// nothing more: the body is not read, nothing is stored, and the response is not
// yet configurable. Phase 1 replaces this with the real thing -- a capped body
// read, ordered headers, raw bytes, and a row in Postgres.
//
// It deliberately does NOT read the body yet. Reading an unbounded body from an
// untrusted client is the vulnerability the brief for Phase 1 opens with, and
// writing the naive version here first would mean shipping it, however briefly.
func (h *Handler) capture(w http.ResponseWriter, r *http.Request) {
	slug := SlugFrom(r.Context())

	h.log.Info("capture (stub)",
		"inbox", slug,
		"method", r.Method,
		"path", r.URL.Path,
		"content_type", r.Header.Get("Content-Type"),
		"content_length", r.ContentLength,
	)

	// 200 with a small body. A provider treats any 2xx as delivered; anything
	// else and it retries, in Stripe's case for up to three days.
	writeJSON(w, http.StatusOK, map[string]any{
		"captured": false,
		"inbox":    slug,
		"method":   r.Method,
		"path":     r.URL.Path,
		"note":     "phase 0 stub: routing works, storage lands in phase 1",
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
