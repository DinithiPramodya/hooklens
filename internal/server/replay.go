package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/DinithiPramodya/hooklens/internal/capture"
	"github.com/DinithiPramodya/hooklens/internal/replay"
	"github.com/DinithiPramodya/hooklens/internal/store"
	"github.com/DinithiPramodya/hooklens/internal/tunnel"
)

// replayTimeout bounds one replay, whichever destination it goes to.
const replayTimeout = 30 * time.Second

// handleReplay resends a stored capture.
//
// Two destinations: the inbox's tunnel (the common case -- iterate on a
// handler without re-triggering the event) or an arbitrary URL. The second
// is why internal/replay is mostly SSRF defence; see
// docs/learn/27-replay-and-ssrf.md.
func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
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
		// Target is "tunnel" (the default) or an http(s) URL.
		Target string `json:"target"`
		// BodyB64 replaces the stored body when present. Base64 for the same
		// reason the capture is: it may be arbitrary bytes.
		BodyB64 *string `json:"body_b64"`
		// Headers replaces the stored headers entirely when present.
		// Replacing rather than merging: a partial merge has no obvious
		// semantics for a repeated header, and "send exactly these" is what
		// an editor produces anyway.
		Headers []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"headers"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	// Start from what was captured, then apply the edits.
	payload := req.Body
	edited := false
	if body.BodyB64 != nil {
		decoded, derr := base64.StdEncoding.DecodeString(*body.BodyB64)
		if derr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body_b64 is not valid base64"})
			return
		}
		payload = decoded
		edited = true
	}

	headers := req.Headers
	if body.Headers != nil {
		headers = make([]capture.Header, len(body.Headers))
		for i, h := range body.Headers {
			headers[i] = capture.Header{Name: h.Name, Value: h.Value}
		}
		edited = true
	}

	ctx, cancel := context.WithTimeout(r.Context(), replayTimeout)
	defer cancel()

	out := map[string]any{
		"replayed": true,
		"edited":   edited,
	}
	// Said whenever the body changed, because the alternative is a developer
	// concluding their verification code broke. An edited body can never
	// match the original MAC -- that is what a MAC is for.
	if edited {
		out["signature_note"] = "The body or headers were edited, so any signature header " +
			"no longer matches. That is expected."
	}

	if body.Target == "" || body.Target == "tunnel" {
		s.replayToTunnel(ctx, w, req, headers, payload, out)
		return
	}
	s.replayToURL(ctx, w, req, body.Target, headers, payload, out)
}

func (s *Server) replayToTunnel(ctx context.Context, w http.ResponseWriter,
	req *store.StoredRequest, headers []capture.Header, payload []byte, out map[string]any) {

	th := make([]tunnel.Header, 0, len(headers)+1)
	for _, h := range headers {
		th = append(th, tunnel.Header{Name: h.Name, Value: h.Value})
	}
	// Marked, always. A replay is byte-identical downstream, so without this
	// a developer's own logs cannot tell "the provider sent it twice" from
	// "I resent it" -- which is the confusion this tool exists to remove.
	th = append(th, tunnel.Header{Name: replay.ReplayHeader, Value: "1"})

	resp, err := s.tunnel.Hub().Forward(ctx, req.EndpointID, tunnel.Request{
		Method:  req.Method,
		Path:    req.Path,
		Query:   req.Query,
		Headers: th,
		BodyB64: base64.StdEncoding.EncodeToString(payload),
	})
	out["target"] = "tunnel"

	if err != nil {
		out["replayed"] = false
		out["error"] = err.Error()
		if errors.Is(err, tunnel.ErrNoTunnel) {
			out["hint"] = "No tunnel is connected. Run `hooklens forward --to localhost:3000`, " +
				"or replay to a URL instead."
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if resp.Error != "" {
		out["replayed"] = false
		out["error"] = resp.Error
		out["hint"] = "The tunnel is up but your local app could not be reached."
		writeJSON(w, http.StatusOK, out)
		return
	}

	out["status"] = resp.Status
	out["headers"] = resp.Headers
	out["body_b64"] = resp.BodyB64
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) replayToURL(ctx context.Context, w http.ResponseWriter,
	req *store.StoredRequest, target string, headers []capture.Header, payload []byte, out map[string]any) {

	out["target"] = target

	// Checked before dialling for the error message; the dial guard inside
	// SafeClient is the control that actually cannot be raced.
	if err := replay.CheckURL(target); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": err.Error(),
			"hint": "Replay to `tunnel` to reach your own machine. Arbitrary URLs are " +
				"restricted to public addresses so this server cannot be used to reach " +
				"private networks on someone else's behalf.",
		})
		return
	}

	rh := make([]replay.Header, len(headers))
	for i, h := range headers {
		rh[i] = replay.Header{Name: h.Name, Value: h.Value}
	}

	resp, err := replay.ToURL(ctx, s.replayClient, target, replay.Request{
		Method:  req.Method,
		Path:    req.Path,
		Query:   req.Query,
		Headers: rh,
		Body:    payload,
	})
	if err != nil {
		var blocked *replay.ErrBlockedDestination
		if errors.As(err, &blocked) {
			// A hostname that resolved somewhere private. Caught at the dial,
			// which is the point of doing it there.
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": err.Error(),
				"hint":  "That hostname resolves to an address this server will not connect to.",
			})
			return
		}
		out["replayed"] = false
		out["error"] = tunnel.ShortError(err)
		writeJSON(w, http.StatusOK, out)
		return
	}

	out["status"] = resp.Status
	out["headers"] = resp.Headers
	out["body_b64"] = base64.StdEncoding.EncodeToString(resp.Body)
	out["elapsed_ms"] = resp.Elapsed.Milliseconds()
	writeJSON(w, http.StatusOK, out)
}
