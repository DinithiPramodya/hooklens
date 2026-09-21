package tunnel

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// localTimeout bounds one request to the developer's app.
//
// Shorter than the server's 30s forward deadline on purpose. Whoever gives up
// first decides what gets recorded, and the client is the only party that can
// say something useful -- "your app did not answer in 25s" beats the server's
// "the tunnel did not answer", which is true but points at the wrong thing.
const localTimeout = 25 * time.Second

// ClientOptions configures one forwarding session.
type ClientOptions struct {
	// ServerURL is the hooklens server, http:// or https://. Converted to
	// ws:// or wss:// internally so a user never has to think about scheme.
	ServerURL string
	Slug      string
	Token     string
	// Target is where requests go: "localhost:3000", or a full URL when a
	// scheme or path prefix is needed.
	Target string
	Log    *slog.Logger
	// OnConnect is called with the public URL each time the handshake
	// succeeds -- including after a reconnect, so the CLI can say it is back.
	// A callback rather than a return value so the banner appears the moment
	// it is true, not after Run returns.
	OnConnect func(publicURL string)
	// OnDisconnect is called when a connection ends and another attempt is
	// coming, with the reason and how long until the retry.
	OnDisconnect func(err error, retryIn time.Duration)
}

// Client is one CLI session against one inbox.
type Client struct {
	opt    ClientOptions
	target *url.URL
	http   *http.Client
	log    *slog.Logger
	// sem bounds concurrent requests to the LOCAL app. The goroutine count
	// is already bounded by the server's per-tunnel limit; this exists
	// because a development server is often single-threaded.
	sem semaphore
}

// NewClient validates the options and builds the HTTP client used for local
// requests.
func NewClient(opt ClientOptions) (*Client, error) {
	target, err := parseTarget(opt.Target)
	if err != nil {
		return nil, err
	}
	if opt.Log == nil {
		opt.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &Client{
		opt:    opt,
		target: target,
		log:    opt.Log,
		sem:    newSemaphore(maxInFlightPerTunnel),
		http: &http.Client{
			Timeout: localTimeout,
			// Do NOT follow redirects. Go follows them by default, which would
			// silently replace the developer's 302 with whatever is at the
			// other end -- and a 302 is a real answer the provider should see.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				// Go's transport otherwise adds Accept-Encoding: gzip and
				// transparently decompresses the reply, so the bytes relayed
				// would not be the bytes the app sent while Content-Encoding
				// still claimed they were. Disabling it means whatever the
				// provider asked for is passed through untouched.
				DisableCompression: true,
				// A loopback address needs no connection pool of any size, but
				// a burst of webhooks should not open a socket each.
				MaxIdleConnsPerHost: 16,
			},
		},
	}, nil
}

// parseTarget accepts "localhost:3000", "http://localhost:3000" and
// "http://localhost:3000/base".
//
// The bare host:port form is what people actually type, and url.Parse reads it
// as a scheme of "localhost" with an opaque path of "3000" -- valid, and
// completely wrong. Detecting the missing scheme is therefore not politeness,
// it is the difference between working and failing confusingly.
func parseTarget(s string) (*url.URL, error) {
	if s == "" {
		return nil, fmt.Errorf("no target: pass --to localhost:3000")
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("bad target %q: %w", s, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("bad target %q: no host", s)
	}
	return u, nil
}

// Run keeps a tunnel up until ctx is cancelled.
//
// It reconnects on failure with exponential backoff and full jitter (failure
// mode 6), reusing the same slug, so the public URL survives a network flap
// without the CLI being restarted. It returns nil for an orderly shutdown and
// an error only when retrying cannot help.
func (c *Client) Run(ctx context.Context) error {
	bo := newBackoff()

	for {
		start := time.Now()
		err := c.connectOnce(ctx)
		session := time.Since(start)

		if ctx.Err() != nil {
			return nil // interrupted; not a failure
		}

		// A connection that lasted counts as success, whatever ended it.
		// Resetting on connect instead would let a server that accepts and
		// instantly drops produce a tight loop of "successful" attempts --
		// see the note on stableSession.
		if session >= stableSession {
			bo.reset()
		}

		var ce *CloseError
		if errors.As(err, &ce) && ce.Permanent() {
			// The client is wrong and waiting will not change that.
			return err
		}

		delay := bo.next()
		if c.opt.OnDisconnect != nil {
			c.opt.OnDisconnect(err, delay)
		}
		c.log.Info("reconnecting", "after", delay.Round(time.Millisecond), "err", err)

		if !sleep(ctx, delay) {
			return nil // cancelled during the wait
		}
	}
}

// connectOnce dials, handshakes, and serves until the connection ends.
func (c *Client) connectOnce(ctx context.Context) error {
	wsURL, err := websocketURL(c.opt.ServerURL)
	if err != nil {
		return err
	}

	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	conn, _, err := websocket.Dial(dialCtx, wsURL, nil)
	cancel()
	if err != nil {
		return fmt.Errorf("connect to %s: %w", c.opt.ServerURL, err)
	}
	defer conn.CloseNow()

	// Response bodies from the local app go OUT, but request bodies come IN,
	// so the read limit has to accommodate a capture plus base64 expansion --
	// matching the server's own limit.
	conn.SetReadLimit(responseReadLimit)

	ack, err := c.handshake(ctx, conn)
	if err != nil {
		return err
	}
	if c.opt.OnConnect != nil {
		c.opt.OnConnect(ack.PublicURL)
	}

	return c.serve(ctx, conn)
}

func (c *Client) handshake(ctx context.Context, conn *websocket.Conn) (*HelloOK, error) {
	hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	hello, err := Encode(TypeHello, Hello{
		Slug: c.opt.Slug, Token: c.opt.Token, Version: ProtocolVersion,
	})
	if err != nil {
		return nil, err
	}
	if err := conn.Write(hctx, websocket.MessageText, hello); err != nil {
		return nil, fmt.Errorf("send hello: %w", err)
	}

	_, data, err := conn.Read(hctx)
	if err != nil {
		return nil, fmt.Errorf("read hello response: %w", err)
	}
	env, err := DecodeEnvelope(data)
	if err != nil {
		return nil, err
	}

	switch env.Type {
	case TypeHelloOK:
		var ok HelloOK
		if err := DecodePayload(env, &ok); err != nil {
			return nil, err
		}
		return &ok, nil

	case TypeClose:
		// The server's reason, printed verbatim. This is why unit 18 sent an
		// application-level close frame instead of relying on the WebSocket
		// close reason: there is room here for a sentence that tells the user
		// what to do.
		var cl Close
		if err := DecodePayload(env, &cl); err != nil {
			return nil, &CloseError{Code: "unknown", Reason: "rejected by server"}
		}
		return nil, &CloseError{Code: cl.Code, Reason: cl.Reason}

	default:
		return nil, fmt.Errorf("unexpected %s frame during handshake", env.Type)
	}
}

// serve is the read loop. There is exactly one, for the same reason the server
// has exactly one: the library forbids concurrent Read, and control frames --
// including the pong answering the server's keepalive -- are only processed
// while a read is in flight.
func (c *Client) serve(ctx context.Context, conn *websocket.Conn) error {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil // interrupted; an orderly stop
			}
			return fmt.Errorf("connection lost: %w", err)
		}

		env, err := DecodeEnvelope(data)
		if err != nil {
			c.log.Warn("undecodable frame from server", "err", err)
			continue
		}

		switch env.Type {
		case TypeRequest:
			var req Request
			if err := DecodePayload(env, &req); err != nil {
				c.log.Warn("undecodable request frame", "err", err)
				continue
			}
			// A goroutine per request, so a slow local handler does not stall
			// the read loop and every other request behind it -- the same
			// reason the server does not serialise.
			//
			// The concurrency bound is taken INSIDE that goroutine, never
			// here. Acquiring on this line would block the read loop while
			// the local app is busy, and a blocked read loop stops processing
			// control frames -- including the pong the server's keepalive is
			// waiting for. The server would then conclude the CLI is dead
			// when it is merely busy, drop the tunnel, and turn a slow
			// handler into an outage.
			//
			// The goroutine count is bounded anyway, by the server's own
			// per-tunnel limit: it will not have more than that many requests
			// outstanding. This semaphore exists to protect the local app,
			// which is frequently a single-threaded development server.
			go c.handle(ctx, conn, req)

		case TypeClose:
			var cl Close
			if err := DecodePayload(env, &cl); err == nil {
				return &CloseError{Code: cl.Code, Reason: cl.Reason}
			}
			return &CloseError{Code: "unknown", Reason: "server closed the tunnel"}

		default:
			c.log.Warn("unexpected frame type from server", "type", env.Type)
		}
	}
}

// handle performs one local request and sends the response frame.
func (c *Client) handle(ctx context.Context, conn *websocket.Conn, req Request) {
	start := time.Now()

	// Bounded here, off the read loop. The wait is capped by the local
	// timeout, so a request that never gets a slot answers with an Error
	// rather than sitting forever -- the server's deadline would give up on
	// it anyway, and a reply it can no longer deliver is wasted work.
	actx, cancel := context.WithTimeout(ctx, localTimeout)
	defer cancel()

	resp := c.callLocalBounded(actx, req)
	resp.ReqID = req.ReqID

	if resp.Error != "" {
		c.log.Info("forward failed", "method", req.Method, "path", req.Path,
			"err", resp.Error, "ms", time.Since(start).Milliseconds())
	} else {
		c.log.Info("forwarded", "method", req.Method, "path", req.Path,
			"status", resp.Status, "ms", time.Since(start).Milliseconds())
	}

	b, err := Encode(TypeResponse, resp)
	if err != nil {
		c.log.Error("encode response frame", "err", err)
		return
	}
	// Its own deadline: the write must not inherit a request context that may
	// already be done, or the answer never leaves.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := conn.Write(wctx, websocket.MessageText, b); err != nil {
		c.log.Warn("send response frame", "err", err)
	}
}

// callLocalBounded takes a concurrency slot, then issues the request.
//
// Separated from callLocal purely so the release can be `defer`red at the
// point of acquisition, which is the rule that keeps a missed release from
// silently shrinking the limit forever.
func (c *Client) callLocalBounded(ctx context.Context, req Request) Response {
	if err := c.sem.acquire(ctx); err != nil {
		// Never got a slot. An Error, not a status, for the same reason an
		// unreachable app is: the local app was never asked.
		return Response{Error: "hooklens client is at its concurrency limit"}
	}
	defer c.sem.release()
	return c.callLocal(ctx, req)
}

// callLocal issues the request against the developer's app.
//
// It never returns an error: a failure to reach the app IS the answer, carried
// in Response.Error, and the server depends on that being distinguishable from
// a status. See docs/learn/20-forwarding.md.
func (c *Client) callLocal(ctx context.Context, req Request) Response {
	body, err := base64.StdEncoding.DecodeString(req.BodyB64)
	if err != nil {
		return Response{Error: "server sent an undecodable body: " + err.Error()}
	}

	target := *c.target
	// The target's own path is a prefix, so --to localhost:3000/api works.
	target.Path = strings.TrimSuffix(target.Path, "/") + req.Path
	target.RawQuery = req.Query

	hreq, err := http.NewRequestWithContext(ctx, req.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return Response{Error: "could not build local request: " + err.Error()}
	}

	for _, h := range req.Headers {
		switch strings.ToLower(h.Name) {
		case "host":
			// The provider addressed our domain. Preserved for an app that
			// wants it, but NOT used as the Host -- frameworks that validate
			// Host (Django's ALLOWED_HOSTS, Rails host authorisation) would
			// reject the request, producing a 400 from the developer's own app
			// for a reason that looks inexplicable.
			hreq.Header.Set("X-Forwarded-Host", h.Value)
		case "content-length":
			// Recomputed by net/http for the body we are actually sending.
			// Copying the original produces a truncated or hanging request.
		default:
			if isHopByHopHeader(h.Name) {
				// Described the connection that ended at our server.
				continue
			}
			hreq.Header.Add(h.Name, h.Value)
		}
	}
	hreq.Header.Set("X-Forwarded-By", "hooklens")

	hresp, err := c.http.Do(hreq)
	if err != nil {
		// Connection refused, DNS failure, our own timeout. Reported as an
		// Error, never as a status: the server turns this into a recorded
		// "unreachable" and still answers the provider with a 2xx, so a local
		// app that is simply not running does not make a provider disable the
		// endpoint.
		return Response{Error: ShortError(err)}
	}
	defer hresp.Body.Close()

	// Bounded, for the same reason every read in this codebase is bounded.
	// A local app streaming forever would otherwise consume the laptop.
	respBody, err := io.ReadAll(io.LimitReader(hresp.Body, responseReadLimit))
	if err != nil {
		return Response{Error: "reading local response: " + err.Error()}
	}

	out := Response{
		Status:  hresp.StatusCode,
		BodyB64: base64.StdEncoding.EncodeToString(respBody),
	}
	for name, values := range hresp.Header {
		if isHopByHopHeader(name) {
			continue
		}
		for _, v := range values {
			out.Headers = append(out.Headers, Header{Name: name, Value: v})
		}
	}
	return out
}

// ShortError turns Go's layered error text into one line a developer can
// act on.
//
// "Get \"http://localhost:3000/hook\": dial tcp 127.0.0.1:3000: connect:
// connection refused" is accurate and nobody reads past the first clause. The
// actionable part is the last one.
func ShortError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 && i+2 < len(msg) {
		last := msg[i+2:]
		if last != "" {
			return last
		}
	}
	return msg
}

// isHopByHopHeader duplicates the list in internal/ingest deliberately.
//
// The two are different directions -- this one strips headers going TO the
// local app, that one strips them coming back FROM it -- and a shared helper
// would create a dependency between the tunnel protocol and the capture
// handler for the sake of nine strings.
func isHopByHopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

// websocketURL converts an http(s) server URL into the ws(s) endpoint.
func websocketURL(server string) (string, error) {
	if server == "" {
		return "", fmt.Errorf("no server URL")
	}
	if !strings.Contains(server, "://") {
		server = "http://" + server
	}
	u, err := url.Parse(server)
	if err != nil {
		return "", fmt.Errorf("bad server URL %q: %w", server, err)
	}
	switch u.Scheme {
	case "http", "ws":
		u.Scheme = "ws"
	case "https", "wss":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported scheme %q in %q", u.Scheme, server)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/api/tunnel"
	u.RawQuery = ""
	return u.String(), nil
}
