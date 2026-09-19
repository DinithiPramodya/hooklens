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

	"github.com/DinithiPramodya/hooklens/internal/capture"
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
	maxBody int64
}

func New(log *slog.Logger, maxBody int64) *Handler {
	if maxBody <= 0 {
		maxBody = capture.DefaultMaxBody
	}
	return &Handler{log: log, maxBody: maxBody}
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

// capture reads the request and (from unit 07) stores it.
func (h *Handler) capture(w http.ResponseWriter, r *http.Request) {
	slug := SlugFrom(r.Context())

	req, err := capture.FromHTTP(r, h.maxBody)
	if err != nil {
		// A body read failure is the client's doing -- they hung up, or timed
		// out -- not ours. Log it and carry on with whatever did arrive: for an
		// inspector, "the sender disconnected after 5 bytes" is the answer the
		// user came for, not an error to swallow.
		h.log.Warn("partial body", "inbox", slug, "err", err)
	}
	if req == nil {
		// Only reachable if FromHTTP failed before constructing anything.
		writeJSON(w, http.StatusOK, map[string]any{"captured": false, "inbox": slug})
		return
	}

	h.log.Info("captured",
		"inbox", slug,
		"method", req.Method,
		"path", req.Path,
		"headers", len(req.Headers),
		"body_bytes", len(req.Body),
		"truncated", req.Truncated,
		"declared_size", req.DeclaredSize,
		"source_ip", req.SourceIP.String(),
	)

	// The response is written LAST, after everything fallible is done. Once we
	// say 200 the provider considers the event delivered and will not send it
	// again, so nothing that can fail may run between the durable record and
	// this line. Today there is no durable record yet -- unit 07 adds it, and
	// the insert goes immediately above this.
	writeJSON(w, http.StatusOK, map[string]any{
		"captured":      false,
		"inbox":         slug,
		"method":        req.Method,
		"path":          req.Path,
		"query":         req.Query,
		"headers":       len(req.Headers),
		"body_bytes":    len(req.Body),
		"truncated":     req.Truncated,
		"declared_size": req.DeclaredSize,
		"note":          "read faithfully; storage lands in unit 07",
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
