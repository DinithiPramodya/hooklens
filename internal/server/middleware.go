package server

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"
)

// requestIDHeader is echoed on every response so a user reporting "request
// abc123 did the wrong thing" gives us something greppable in the logs.
const requestIDHeader = "X-Request-Id"

// statusRecorder wraps a ResponseWriter to remember what was written.
//
// net/http gives no way to ask a ResponseWriter what status it sent, so logging
// middleware has to record it on the way past. The zero value of status is 0,
// not 200, which is how we distinguish "the handler called WriteHeader(200)"
// from "the handler never wrote anything" -- see WriteHeader below.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (rec *statusRecorder) WriteHeader(status int) {
	if rec.status == 0 {
		rec.status = status
	}
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *statusRecorder) Write(b []byte) (int, error) {
	// A handler that calls Write without WriteHeader gets an implicit 200. We
	// have to mirror that here or such responses would log as status 0.
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.bytes += int64(n)
	return n, err
}

// withRequestLog assigns a request ID and logs one line per completed request.
func withRequestLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		id := newRequestID()
		w.Header().Set(requestIDHeader, id)

		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		// A handler that returns without writing anything leaves status 0.
		// net/http sends 200 in that case, so report what the client saw.
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}

		log.Info("request",
			"id", id,
			"method", r.Method,
			"host", r.Host,
			"path", r.URL.Path,
			"status", status,
			"bytes", rec.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}

// withRecover turns a panic in a handler into a 500 instead of a dead connection.
//
// net/http already recovers panics per-connection, so a panic will not take the
// process down. What it will do is close the connection with no response at all,
// which the client sees as a network error rather than a server error, and log a
// stack trace to the default logger rather than ours. This makes the failure
// legible on both sides.
func withRecover(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler is the documented way for a handler to
			// abandon a response deliberately. Re-panic so net/http handles it
			// as intended instead of logging it as a crash.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			log.Error("panic in handler",
				"id", w.Header().Get(requestIDHeader),
				"path", r.URL.Path,
				"panic", rec,
			)
			// If the handler already wrote a header this is a no-op with a
			// "superfluous WriteHeader" warning, which is the correct outcome:
			// the client has a response, it is just not the one we wanted.
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}

// newRequestID returns 16 hex characters of randomness.
//
// crypto/rand rather than math/rand, and not because request IDs are a secret:
// since Go 1.24 crypto/rand.Read cannot fail and needs no seeding, so it is
// simply the one with no failure path to handle.
func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
