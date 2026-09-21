package tunnel

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
)

// Defaults for the connection's lifecycle.
//
// handshakeTimeout is a security bound, not a convenience. The HTTP upgrade
// completes BEFORE the client has authenticated -- authentication is the
// first frame, not a header -- so between Accept and the hello there is a
// held-open connection belonging to nobody. Without this deadline, opening
// thousands of connections and never speaking costs an attacker nothing and
// costs us a file descriptor and a goroutine each.
//
// pingInterval is failure mode 7 from PLAN.md: NAT and proxy idle timeouts are
// not advertised and can be as short as 30 seconds (see
// docs/learn/17-nat-and-firewalls.md), so the connection has to generate
// traffic to stay alive. 20s leaves room under a 30s reaper.
//
// pongTimeout is how long a ping waits before the peer is declared gone. It
// must exceed a plausible round trip on a bad mobile connection, and stay
// under pingInterval + itself so a dead peer is detected inside one cycle.
const (
	defaultHandshakeTimeout = 10 * time.Second
	defaultPingInterval     = 20 * time.Second
	defaultPongTimeout      = 25 * time.Second

	// The hello frame is tiny. A read limit this low means a client that opens
	// a connection and streams megabytes instead of authenticating is cut off
	// by the library rather than by us noticing. Raised after the handshake,
	// once we know who is on the other end and response bodies start arriving.
	handshakeReadLimit = 4 << 10
)

// ErrUnauthorized is what an AuthFunc returns for a bad slug or token. It is
// deliberately one error for both, matching the HTTP API: distinguishing them
// turns the endpoint into an oracle for which inboxes exist. See
// docs/learn/08-capability-urls.md.
var ErrUnauthorized = errors.New("unauthorized")

// AuthFunc validates a slug and token, returning the endpoint's id.
//
// A function rather than an interface over the store, and declared here in the
// consumer rather than beside the implementation. This package therefore has
// no dependency on internal/store, which keeps it testable with a two-line
// stub and keeps the protocol independent of how endpoints happen to be
// persisted.
type AuthFunc func(ctx context.Context, slug, token string) (endpointID string, err error)

// Server is the server half of the tunnel protocol: it accepts WebSocket
// upgrades, authenticates them, and holds the connections open.
type Server struct {
	log       *slog.Logger
	auth      AuthFunc
	publicURL func(slug string) string

	handshakeTimeout time.Duration
	pingInterval     time.Duration
	pongTimeout      time.Duration

	// baseCtx is cancelled when the process is shutting down.
	//
	// It exists because http.Server.Shutdown does NOT wait for, or even know
	// about, hijacked connections -- and a WebSocket is a hijacked connection.
	// Without this, a restart would drop every tunnel abruptly with no close
	// frame, and each CLI would discover it only when its next ping failed.
	baseCtx context.Context
}

// Options carries the tunables. Zero values mean "use the default", so a
// caller passes only what it wants to change and tests can shrink the
// timeouts without knowing the rest.
type Options struct {
	HandshakeTimeout time.Duration
	PingInterval     time.Duration
	PongTimeout      time.Duration
}

func New(baseCtx context.Context, log *slog.Logger, auth AuthFunc, publicURL func(string) string, opt Options) *Server {
	s := &Server{
		log:              log,
		auth:             auth,
		publicURL:        publicURL,
		baseCtx:          baseCtx,
		handshakeTimeout: firstNonZero(opt.HandshakeTimeout, defaultHandshakeTimeout),
		pingInterval:     firstNonZero(opt.PingInterval, defaultPingInterval),
		pongTimeout:      firstNonZero(opt.PongTimeout, defaultPongTimeout),
	}
	return s
}

func firstNonZero(v, fallback time.Duration) time.Duration {
	if v != 0 {
		return v
	}
	return fallback
}

// Handle upgrades an HTTP request to a tunnel connection.
//
// It does not return until the connection is finished, which for a healthy
// tunnel is hours. That is normal for a hijacked connection and is why the
// shutdown path runs off baseCtx rather than off the HTTP server.
func (s *Server) Handle(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// No compression. Payloads are mostly small JSON control frames, and
		// permessage-deflate holds a compression context per connection --
		// real memory per idle tunnel, to save bytes on traffic that is
		// already tiny. Revisit if large bodies dominate.
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		// Accept has already written an HTTP error response. Anything we write
		// now would be a second response, so this can only be logged.
		s.log.Debug("tunnel upgrade rejected", "err", err, "remote", r.RemoteAddr)
		return
	}
	// CloseNow, not Close: this is the "something went wrong" path that runs
	// on every return. Close performs a closing handshake and can block; by
	// the time this defer runs we have either already closed politely or we
	// are giving up, and neither wants to wait on a peer that may be gone.
	defer c.CloseNow()

	c.SetReadLimit(handshakeReadLimit)

	// Derived from baseCtx, NOT from r.Context(). Once the connection is
	// hijacked the HTTP server no longer manages it, so r.Context() is no
	// longer a reliable signal that anything is wrong -- but it is still tied
	// to the request, and we need a lifetime tied to the process instead.
	ctx, cancel := context.WithCancel(s.baseCtx)
	defer cancel()

	ep, slug, ok := s.handshake(ctx, c, r)
	if !ok {
		return
	}

	log := s.log.With("inbox", slug, "endpoint_id", ep, "remote", r.RemoteAddr)
	log.Info("tunnel connected")
	defer log.Info("tunnel disconnected")

	s.serve(ctx, c, log)
}

// handshake reads the hello frame, authenticates it, and answers. It reports
// whether the connection may proceed.
func (s *Server) handshake(ctx context.Context, c *websocket.Conn, r *http.Request) (endpointID, slug string, ok bool) {
	// The obvious way to bound this read is context.WithTimeout on the Read
	// call. It does not work, and the way it fails is worth knowing: this
	// library CLOSES THE CONNECTION when a read's context is cancelled. So the
	// deadline that detects the timeout also destroys the socket we wanted to
	// send the explanation on, and the peer gets an unexplained EOF. A test
	// asserting that the close frame carries a reason is what found it.
	//
	// Instead the read runs on the connection's own context and a timer races
	// it. The channel is buffered so the goroutine can always deliver its
	// result and exit, even on the branch where nobody is left to receive.
	type helloRead struct {
		typ  websocket.MessageType
		data []byte
		err  error
	}
	ch := make(chan helloRead, 1)
	go func() {
		typ, data, err := c.Read(ctx)
		ch <- helloRead{typ, data, err}
	}()

	timer := time.NewTimer(s.handshakeTimeout)
	defer timer.Stop()

	var typ websocket.MessageType
	var data []byte
	select {
	case <-timer.C:
		// The connection is still open here, which is the entire point: the
		// close frame can still be written. Writing it also unblocks the read
		// goroutine above, which then exits.
		s.log.Debug("tunnel handshake timed out", "remote", r.RemoteAddr)
		s.closeWith(c, CodeHandshake, "no hello frame within the handshake deadline")
		return "", "", false

	case res := <-ch:
		if res.err != nil {
			// The peer hung up or the process is shutting down. Nobody has
			// authenticated, so there is nobody known to explain anything to.
			s.log.Debug("tunnel read hello", "err", res.err, "remote", r.RemoteAddr)
			return "", "", false
		}
		typ, data = res.typ, res.data
	}

	if typ != websocket.MessageText {
		s.closeWith(c, CodeMalformed, "frames must be text, not binary")
		return "", "", false
	}

	env, err := DecodeEnvelope(data)
	if err != nil {
		s.closeWith(c, CodeMalformed, err.Error())
		return "", "", false
	}
	if env.Type != TypeHello {
		s.closeWith(c, CodeMalformed, "first frame must be hello, got "+string(env.Type))
		return "", "", false
	}

	var hello Hello
	if err := DecodePayload(env, &hello); err != nil {
		s.closeWith(c, CodeMalformed, err.Error())
		return "", "", false
	}

	// Version before credentials. An old client failing on authentication
	// would send its owner hunting for a token problem they do not have.
	if hello.Version != ProtocolVersion {
		s.closeWith(c, CodeVersion,
			"this server speaks protocol version "+strconv.Itoa(ProtocolVersion)+
				", your client speaks "+strconv.Itoa(hello.Version)+" — upgrade the CLI")
		return "", "", false
	}

	id, err := s.auth(ctx, hello.Slug, hello.Token)
	if err != nil {
		if !errors.Is(err, ErrUnauthorized) {
			// A database failure is ours, not theirs. Logged at error, and the
			// client is still told only "unauthorized" -- an internal detail
			// here would leak whether the slug exists.
			s.log.Error("tunnel authenticate", "err", err, "remote", r.RemoteAddr)
		}
		s.closeWith(c, CodeUnauthorized, "unknown inbox or bad token")
		return "", "", false
	}

	ack, err := Encode(TypeHelloOK, HelloOK{
		Slug:        hello.Slug,
		PublicURL:   s.publicURL(hello.Slug),
		PingSeconds: int(s.pingInterval / time.Second),
	})
	if err != nil {
		s.log.Error("tunnel encode hello_ok", "err", err)
		return "", "", false
	}
	if err := c.Write(ctx, websocket.MessageText, ack); err != nil {
		s.log.Debug("tunnel write hello_ok", "err", err)
		return "", "", false
	}

	return id, hello.Slug, true
}

// serve holds an authenticated connection open until it dies.
func (s *Server) serve(ctx context.Context, c *websocket.Conn, log *slog.Logger) {
	// Buffered with room for exactly the one value this goroutine sends.
	// Unbuffered, a reader that finishes after we have stopped selecting --
	// which is every path where the ping loop exits first -- would block
	// forever on the send and leak the goroutine and the connection with it.
	readErr := make(chan error, 1)
	// WithoutCancel, for the same reason the handshake does not put a deadline
	// on its read: a cancelled read context closes the connection, so a reader
	// watching ctx would tear the socket down the instant the process began
	// shutting down -- before the branch below could write the close frame
	// explaining why.
	//
	// So the reader's exit condition is not a context, it is the connection
	// itself closing. Cancelling ctx ends this supervisor loop, the supervisor
	// closes the connection, and the closed connection ends the reader. One
	// direction, no race.
	go func() { readErr <- s.readLoop(context.WithoutCancel(ctx), c) }()

	ticker := time.NewTicker(s.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Process shutdown. The peer is told why, so the CLI can print
			// something better than "connection reset".
			s.closeWith(c, CodeServerShutdown, "server is shutting down")
			return

		case err := <-readErr:
			// The read loop is the connection's health signal: a peer that
			// vanishes surfaces here as a read error long before a ping is due.
			if err != nil && !isNormalClose(err) {
				log.Debug("tunnel read loop ended", "err", err)
			}
			return

		case <-ticker.C:
			// Ping waits for the matching pong, so this single call is both
			// the keepalive that stops an idle NAT row expiring and the
			// liveness check that detects a peer which died without closing.
			pctx, cancel := context.WithTimeout(ctx, s.pongTimeout)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				log.Info("tunnel ping failed; treating peer as gone", "err", err)
				return
			}
		}
	}
}

// readLoop consumes frames until the connection ends.
//
// It must keep reading even though this unit has no frame worth acting on yet:
// a WebSocket's control frames -- including the pong that Ping is waiting for
// -- are only processed while a read is in flight. Stop reading and every ping
// times out on a perfectly healthy connection.
func (s *Server) readLoop(ctx context.Context, c *websocket.Conn) error {
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			s.closeWith(c, CodeMalformed, "frames must be text, not binary")
			return nil
		}
		env, err := DecodeEnvelope(data)
		if err != nil {
			s.closeWith(c, CodeMalformed, err.Error())
			return nil
		}
		switch env.Type {
		case TypeResponse:
			// Correlation arrives in unit 19. Until then a response frame has
			// no request to belong to, so it is dropped with a log rather than
			// silently ignored.
			s.log.Debug("tunnel response frame with no pending request (not yet implemented)")
		default:
			s.closeWith(c, CodeMalformed, "unexpected frame type "+string(env.Type))
			return nil
		}
	}
}

// closeWith sends an application-level close frame and then closes the socket.
//
// Two closes, deliberately. The WebSocket close frame carries a reason capped
// at 123 bytes that several intermediaries mangle or drop, so it cannot be
// relied on to explain anything. The application-level frame has room for a
// real sentence and a stable code the CLI can switch on; the protocol close
// that follows is what actually ends the connection cleanly.
func (s *Server) closeWith(c *websocket.Conn, code, reason string) {
	// Its own short deadline, not the connection's context: this runs on paths
	// where that context is already cancelled, and a close frame sent with a
	// dead context would never leave.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.baseCtx), 2*time.Second)
	defer cancel()

	if b, err := Encode(TypeClose, Close{Code: code, Reason: reason}); err == nil {
		// Best effort. If the peer is already gone this fails, and that is
		// exactly the case where there is nothing useful to do about it.
		_ = c.Write(ctx, websocket.MessageText, b)
	}
	_ = c.Close(websocket.StatusNormalClosure, code)
}

func isNormalClose(err error) bool {
	status := websocket.CloseStatus(err)
	return status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway
}
