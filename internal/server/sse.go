package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/DinithiPramodya/hooklens/internal/store"
)

const (
	// heartbeatInterval is how often a comment line is written to an otherwise
	// idle stream. Proxies, load balancers and NAT tables all reap quiet
	// connections; 20s is comfortably under the usual 30-60s reaping windows.
	heartbeatInterval = 20 * time.Second

	// reconnectDelay is sent once as `retry:` so a client that honours it (and
	// EventSource does) waits this long before reconnecting.
	reconnectDelay = 3 * time.Second
)

// sseWriter writes the Server-Sent Events wire format to a response.
//
// It exists as a type rather than a handful of fmt.Fprintf calls because every
// write has to be followed by a flush, and forgetting one produces a stream
// that looks correct on the server and delivers nothing to the client. Putting
// the flush inside each method makes that impossible to forget.
type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

// newSSEWriter sets the response headers and clears the write deadline.
func newSSEWriter(w http.ResponseWriter) (*sseWriter, error) {
	rc := http.NewResponseController(w)

	// The server sets WriteTimeout: 30s, which is right for ordinary requests
	// and fatal for this one -- an SSE stream is meant to live for hours, and
	// would be cut at exactly 30 seconds with no error the client could
	// distinguish from a network fault.
	//
	// Clearing the deadline for THIS connection only is the fix. Removing
	// WriteTimeout globally would strip the protection from every normal
	// response, which is the tempting and wrong version of this change.
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear write deadline: %w", err)
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	// no-store, not no-cache: this response must never be stored at all, not
	// merely revalidated.
	h.Set("Cache-Control", "no-store")
	// nginx buffers proxied responses by default and would hold every event
	// until the stream ends, which is never. This is the documented opt-out.
	// Harmless when nothing is proxying.
	h.Set("X-Accel-Buffering", "no")

	w.WriteHeader(http.StatusOK)

	s := &sseWriter{w: w, rc: rc}
	return s, s.retry(reconnectDelay)
}

// event writes a named event carrying data.
func (s *sseWriter) event(name, data string) error {
	var b strings.Builder
	if name != "" {
		b.WriteString("event: ")
		b.WriteString(name)
		b.WriteString("\n")
	}
	// A data payload containing newlines must be split across multiple data:
	// lines -- the receiver rejoins them with "\n". Emitting a raw newline
	// inside one data: line ends the event early, so a pretty-printed JSON
	// payload would silently truncate at its first line break.
	for line := range strings.SplitSeq(data, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	// The blank line is what terminates the event. Without it the client keeps
	// buffering and dispatches nothing.
	b.WriteString("\n")

	return s.write(b.String())
}

// comment writes a `: text` line. Ignored by every parser, but it is bytes on
// the wire, which is the entire point: it keeps an idle connection from being
// reaped.
func (s *sseWriter) comment(text string) error {
	return s.write(": " + text + "\n\n")
}

// retry tells the client how long to wait before reconnecting.
func (s *sseWriter) retry(d time.Duration) error {
	return s.write(fmt.Sprintf("retry: %d\n\n", d.Milliseconds()))
}

func (s *sseWriter) write(str string) error {
	if _, err := fmt.Fprint(s.w, str); err != nil {
		return err
	}
	// Go buffers response writes. An SSE handler never returns, so without an
	// explicit flush nothing reaches the client until the buffer happens to
	// fill -- which for events this small is effectively never. This single
	// line is the difference between the feature working and appearing to send
	// nothing at all.
	return s.rc.Flush()
}

// handleStream opens an event stream for one inbox.
//
// Authenticated with the ordinary Bearer token, like every other read. That is
// worth noting because it rules out EventSource on the client -- see
// docs/learn/13-server-sent-events.md.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	ep, err := s.store.AuthenticateEndpoint(r.Context(), r.PathValue("slug"), bearerToken(r))
	if errors.Is(err, store.ErrUnauthorized) {
		unauthorized(w)
		return
	}
	if err != nil {
		s.log.Error("authenticate stream", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "lookup failed"})
		return
	}

	// Everything above this line may still write a normal JSON response.
	// Everything below has committed to a 200 and an event stream, so failures
	// can only be logged and the connection closed.
	sse, err := newSSEWriter(w)
	if err != nil {
		s.log.Error("open stream", "inbox", ep.Slug, "err", err)
		return
	}

	// An immediate event, before any heartbeat. It gives the client something
	// to key "connected" off, and it forces the headers and first bytes out so
	// a buffering intermediary reveals itself at once rather than after the
	// first 20-second silence.
	if err := sse.event("connected", `{"inbox":"`+ep.Slug+`"}`); err != nil {
		return
	}

	s.log.Info("stream opened", "inbox", ep.Slug)
	defer s.log.Info("stream closed", "inbox", ep.Slug)

	ticker := time.NewTicker(s.heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			// The client went away: navigated, closed the tab, or the network
			// dropped. r.Context() is cancelled by net/http when the connection
			// closes, which is the only reliable signal -- a write to a dead
			// connection can succeed into a kernel buffer and tell us nothing.
			return

		case <-ticker.C:
			if err := sse.comment("keepalive"); err != nil {
				// A failed write means the connection is gone. Not worth an
				// error log: this is how streams normally end.
				return
			}
		}
	}
}
