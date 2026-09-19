// Package ingest is the capture endpoint: the handler a webhook provider talks
// to. It accepts any method, any path, any headers and any body, because we do
// not get to say what a provider sends -- our job is to record it faithfully.
package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/DinithiPramodya/hooklens/internal/capture"
	"github.com/DinithiPramodya/hooklens/internal/store"
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
	maxBody int64
}

func New(log *slog.Logger, st *store.Store, maxBody int64) *Handler {
	if maxBody <= 0 {
		maxBody = capture.DefaultMaxBody
	}
	return &Handler{log: log, store: st, maxBody: maxBody}
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
	// subdomains. Rejecting first costs one indexed lookup.
	ep, err := h.store.EndpointBySlug(ctx, slug)
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

	// ---- the request is durable from here on ----
	//
	// Everything below this line must be incapable of turning a stored request
	// into a non-2xx response. A provider treats any non-2xx as "not delivered"
	// and retries, so a failure here would produce a duplicate for an event we
	// already hold -- the exact bug from Phase 0 quiz Q1.

	// Best-effort, deliberately ignoring the error: last_seen_at is a nicety.
	if err := h.store.TouchEndpoint(ctx, ep.ID, req.ReceivedAt); err != nil {
		h.log.Warn("touch endpoint failed", "inbox", slug, "err", err)
	}

	h.log.Info("captured",
		"id", id,
		"inbox", slug,
		"method", req.Method,
		"path", req.Path,
		"headers", len(req.Headers),
		"body_bytes", len(req.Body),
		"truncated", req.Truncated,
	)

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
