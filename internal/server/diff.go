package server

import (
	"errors"
	"net/http"

	"github.com/DinithiPramodya/hooklens/internal/capture"
	"github.com/DinithiPramodya/hooklens/internal/diff"
	"github.com/DinithiPramodya/hooklens/internal/store"
)

// handleDiff compares two captures.
//
// GET with the other id in the query string, unlike verify and replay:
// nothing here is a secret, both ids are already authenticated by the same
// token, and a diff is a read. Being able to link to one matters more than
// consistency with the other two endpoints.
func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)

	left, err := s.store.AuthenticateRequest(r.Context(), r.PathValue("id"), token)
	if errors.Is(err, store.ErrUnauthorized) {
		unauthorized(w)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request id"})
		return
	}

	otherID := r.URL.Query().Get("with")
	if otherID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing ?with=<request id>",
		})
		return
	}

	// Authenticated separately, with the same token. Two captures in
	// different inboxes cannot be diffed by someone holding one token,
	// which is the same rule every other read follows -- and checking only
	// the first id would make this endpoint a way to read any capture by
	// diffing it against one you own.
	right, err := s.store.AuthenticateRequest(r.Context(), otherID, token)
	if errors.Is(err, store.ErrUnauthorized) {
		unauthorized(w)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad ?with= request id"})
		return
	}

	body := diff.Bodies(left.Body, right.Body)
	headers := diff.Headers(toDiffHeaders(left.Headers), toDiffHeaders(right.Headers))

	if r.URL.Query().Get("volatile") != "1" {
		// Filtered by default, because a Date and a signature differ on
		// every request and drown the two fields somebody is looking for.
		// Opt IN to seeing them, rather than opting out -- and the response
		// says the filter is on, because a diff that silently hides fields
		// is a diff that lies.
		headers = diff.WithoutVolatile(headers)
	}

	// Request-line differences, which are not headers and not body and are
	// frequently the whole answer: the same payload sent to a different
	// path is a routing bug, not a payload bug.
	var meta []diff.Change
	meta = appendIfDifferent(meta, "method", left.Method, right.Method)
	meta = appendIfDifferent(meta, "path", left.Path, right.Path)
	meta = appendIfDifferent(meta, "query", left.Query, right.Query)

	writeJSON(w, http.StatusOK, map[string]any{
		"left":             left.ID,
		"right":            right.ID,
		"request":          meta,
		"headers":          headers,
		"body":             body.Changes,
		"body_note":        body.Note,
		"identical":        len(meta) == 0 && len(headers) == 0 && body.Identical,
		"volatile_hidden":  r.URL.Query().Get("volatile") != "1",
		"volatile_headers": diff.VolatileHeaders,
	})
}

func appendIfDifferent(out []diff.Change, name, l, r string) []diff.Change {
	if l == r {
		return out
	}
	return append(out, diff.Change{Path: name, Op: diff.Changed, Old: l, New: r})
}

func toDiffHeaders(hs []capture.Header) []diff.Header {
	out := make([]diff.Header, len(hs))
	for i, h := range hs {
		out[i] = diff.Header{Name: h.Name, Value: h.Value}
	}
	return out
}
