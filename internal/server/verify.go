package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/DinithiPramodya/hooklens/internal/capture"
	"github.com/DinithiPramodya/hooklens/internal/signature"
	"github.com/DinithiPramodya/hooklens/internal/store"
)

// handleVerify checks a stored capture's signature against a secret the
// caller supplies.
//
// POST, not GET, for one reason: the secret. A GET would put it in a query
// string, and a query string lands in the server access log, the proxy log,
// the browser's history and the Referer of any outbound link -- which is
// exactly the leak that makes people distrust capability URLs (unit 08).
//
// The secret is used and discarded. It is never stored, never logged, and
// never returned. Holding other people's webhook secrets would make this
// service worth attacking for a reason unrelated to what it does, and there
// is no feature that needs it beyond this call.
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	req, err := s.store.AuthenticateRequest(r.Context(), r.PathValue("id"), bearerToken(r))
	if errors.Is(err, store.ErrUnauthorized) {
		unauthorized(w)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request id"})
		return
	}

	var body struct {
		Secret   string `json:"secret"`
		Provider string `json:"provider"`
	}
	// Bounded like every other read. A secret is short; 8KB is generous.
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if body.Secret == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "a secret is required",
			"hint":  "Send {\"secret\": \"whsec_...\"}. It is used for this check and not stored.",
		})
		return
	}

	headers := toSignatureHeaders(req.Headers)

	provider := signature.Provider(body.Provider)
	if provider == "" {
		detected, ok := signature.Detect(headers)
		if !ok {
			writeJSON(w, http.StatusOK, map[string]any{
				"detected": false,
				"hint": "No signature header this tool recognises. Supported: Stripe-Signature, " +
					"X-Hub-Signature-256, X-Shopify-Hmac-Sha256, X-Slack-Signature.",
			})
			return
		}
		provider = detected
	}

	// req.ReceivedAt, not time.Now(). A stored capture is re-verified against
	// the moment it ARRIVED, or every capture older than the replay window
	// reads as expired -- true, and useless.
	res := signature.Verify(provider, headers, req.Body, []byte(body.Secret), req.ReceivedAt)

	out := map[string]any{
		"detected": true,
		"provider": res.Provider,
		"valid":    res.Valid,
		// The canonical string is the point of the whole endpoint: nearly
		// every real signature bug is a canonical-string bug, and none of
		// them is diagnosable from a boolean.
		"signed_string": res.Signed,
		"expected":      res.Expected,
		"provided":      res.Provided,
	}
	if res.Problem != "" {
		out["problem"] = res.Problem
	}
	if res.Hint != "" {
		out["hint"] = res.Hint
	}
	if !res.Timestamp.IsZero() {
		out["timestamp"] = res.Timestamp
		out["age_seconds"] = int(res.Age.Seconds())
	}
	writeJSON(w, http.StatusOK, out)
}

func toSignatureHeaders(hs []capture.Header) []signature.Header {
	out := make([]signature.Header, len(hs))
	for i, h := range hs {
		out[i] = signature.Header{Name: h.Name, Value: h.Value}
	}
	return out
}
