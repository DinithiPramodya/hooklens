package tunnel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

// The three ways a forward can fail without the local app ever answering.
// Separate errors because the caller reports each one differently to the
// provider and to the UI, and because "no tunnel" is a normal state rather
// than a fault -- see failure mode 1: capture and store anyway.
var (
	ErrNoTunnel     = errors.New("no tunnel connected for this inbox")
	ErrTimeout      = errors.New("tunnel request timed out")
	ErrDisconnected = errors.New("tunnel disconnected while the request was in flight")
	// ErrOverloaded means the tunnel was already at its in-flight limit and a
	// slot did not come free before the deadline.
	//
	// Distinct from ErrTimeout on purpose, and the distinction is the same one
	// this package keeps making: "your app did not answer" and "we never asked
	// it" send a developer to different places.
	ErrOverloaded = errors.New("tunnel is at its in-flight limit")
)

// Hub is the registry of connected tunnels, one per inbox.
//
// It is the only part of this package the rest of the application talks to:
// ingest asks it to forward a captured request and does not know or care
// whether a WebSocket exists.
type Hub struct {
	mu      sync.Mutex
	clients map[string]*client // endpoint id -> the tunnel currently holding it
}

func NewHub() *Hub {
	return &Hub{clients: make(map[string]*client)}
}

// Forward hands a captured request to the inbox's tunnel and waits for the
// answer, or for ctx to expire.
//
// The deadline belongs to the caller, not to this package: ingest knows how
// long a provider is willing to wait, and this code does not.
func (h *Hub) Forward(ctx context.Context, endpointID string, req Request) (*Response, error) {
	h.mu.Lock()
	c := h.clients[endpointID]
	h.mu.Unlock()

	// Deliberately not holding the hub lock while the request is in flight.
	// That would serialise every forward in the process behind one mutex for
	// the entire duration of a round trip to somebody's laptop.
	if c == nil {
		return nil, ErrNoTunnel
	}
	return c.send(ctx, req)
}

// Connected reports whether an inbox currently has a tunnel.
func (h *Hub) Connected(endpointID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clients[endpointID] != nil
}

// register installs c as the tunnel for endpointID, returning whichever
// client it displaced.
//
// Newest wins -- failure mode 5. The alternative, refusing the second
// connection, is worse in the case that actually happens: a laptop sleeps, the
// old connection is dead but the server has not noticed yet, and refusing
// would lock the developer out of their own inbox until a timeout they cannot
// see elapses.
func (h *Hub) register(endpointID string, c *client) *client {
	h.mu.Lock()
	defer h.mu.Unlock()
	old := h.clients[endpointID]
	h.clients[endpointID] = c
	return old
}

// unregister removes c, but only if c is still the registered client.
//
// The identity check is load-bearing and its absence is a subtle bug. When B
// replaces A, A's deferred unregister still runs -- and without this check it
// would delete B, silently disconnecting the tunnel that just won. Every
// forward afterwards would report "no tunnel connected" while a perfectly
// healthy CLI sat there believing it was live.
func (h *Hub) unregister(endpointID string, c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients[endpointID] == c {
		delete(h.clients, endpointID)
	}
}

// client is one connected tunnel and its in-flight requests.
type client struct {
	conn *websocket.Conn
	log  *slog.Logger

	// nextID numbers requests on this connection. Ids only need to be unique
	// per connection -- the map they key is per connection -- so a counter is
	// enough, and it makes logs far easier to follow than random hex would.
	nextID atomic.Uint64

	mu      sync.Mutex
	pending map[string]chan *Response
	closed  bool

	// done is closed exactly once, when the connection dies.
	//
	// This is what makes failure mode 4 immediate. Without it, a tunnel
	// dropping would leave every blocked caller waiting out its full deadline
	// for an answer that provably cannot arrive -- thirty seconds of a
	// provider holding a connection open for nothing, per in-flight request.
	done chan struct{}

	// sem bounds in-flight requests on this tunnel -- failure mode 9.
	//
	// Per client, not per hub: one inbox being hammered must not starve the
	// others. See docs/learn/23-bounded-concurrency.md.
	sem semaphore
}

func newClient(conn *websocket.Conn, log *slog.Logger) *client {
	return &client{
		conn:    conn,
		log:     log,
		pending: make(map[string]chan *Response),
		done:    make(chan struct{}),
		sem:     newSemaphore(maxInFlightPerTunnel),
	}
}

// send writes a request and waits for the matching response.
func (c *client) send(ctx context.Context, req Request) (*Response, error) {
	// The bound, taken before anything else is allocated. Waiting here rather
	// than rejecting immediately is right because the provider is already
	// waiting and the caller's deadline bounds the wait -- see the three
	// choices in the note.
	if err := c.sem.acquire(ctx); err != nil {
		// The deadline expired while queued, so the request was never sent.
		// Reported as ErrOverloaded rather than ErrTimeout: nothing was asked
		// of the local app, and saying otherwise would send its author
		// debugging a handler that never ran.
		return nil, ErrOverloaded
	}
	// Deferred at the point of acquisition, not at the end of the happy path.
	// A single early return that skipped this would permanently shrink the
	// tunnel's capacity -- silently, cumulatively, and with no error anywhere.
	defer c.sem.release()

	id := strconv.FormatUint(c.nextID.Add(1), 10)
	req.ReqID = id

	// Buffered with room for exactly one value, and that buffer is not an
	// optimisation. If this caller gives up at its deadline and walks away,
	// the reader goroutine delivering a late response would block forever on
	// an unbuffered channel -- and it is the ONLY reader for this connection,
	// so one abandoned response would freeze every other request on the
	// tunnel. With a buffer the send always completes and the value is simply
	// garbage collected.
	ch := make(chan *Response, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrDisconnected
	}
	c.pending[id] = ch
	c.mu.Unlock()

	// Registered on the line above, released here, before anything that can
	// fail. Every exit path below -- encode error, write error, timeout,
	// disconnect, success -- runs this. Attaching the cleanup to the happy
	// path instead is how this pattern leaks a channel and a map entry per
	// request, forever.
	defer c.forget(id)

	b, err := Encode(TypeRequest, req)
	if err != nil {
		return nil, err
	}
	// No write mutex: the library documents that all methods except Read may
	// be called concurrently (conn.go:30). Adding one would serialise every
	// forward on this tunnel behind a lock the library already holds.
	if err := c.conn.Write(ctx, websocket.MessageText, b); err != nil {
		return nil, fmt.Errorf("write request frame: %w", err)
	}

	select {
	case resp := <-ch:
		return resp, nil

	case <-c.done:
		// The tunnel died. Immediate, not at the deadline.
		return nil, ErrDisconnected

	case <-ctx.Done():
		// The local app is hanging -- failure mode 3. The request stays
		// "in flight" as far as the CLI is concerned; we simply stop waiting,
		// and the deferred forget makes the late response undeliverable
		// rather than a leak.
		return nil, ErrTimeout
	}
}

// deliver routes a response frame to whoever is waiting for it.
func (c *client) deliver(resp *Response) {
	c.mu.Lock()
	ch, ok := c.pending[resp.ReqID]
	if ok {
		// Taken AND removed under the same lock. That is what makes the send
		// below safe: a buggy or malicious client echoing the same req_id
		// twice finds nothing the second time, so there can never be two
		// sends competing for one buffer slot.
		delete(c.pending, resp.ReqID)
	}
	c.mu.Unlock()

	if !ok {
		// Nobody is waiting: the caller already timed out, or the id was
		// never issued. Logged rather than ignored -- a steady stream of
		// these means the local app is consistently slower than the deadline.
		c.log.Debug("tunnel response for unknown request", "req_id", resp.ReqID)
		return
	}
	ch <- resp
}

// forget drops a pending entry if it is still there.
//
// Idempotent by design: deliver may already have removed it, and this runs
// unconditionally on every send path.
func (c *client) forget(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, id)
}

// close marks the connection dead and wakes every waiting caller.
func (c *client) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	// Clearing the map is not strictly required -- every sender removes its
	// own entry on the way out -- but it drops the references now rather than
	// holding them until the last slow caller notices, and it makes a leak
	// visible in a test as a non-empty map after close.
	clear(c.pending)
	c.mu.Unlock()

	// Outside the lock, and exactly once thanks to the closed flag above.
	// Closing a channel twice panics, and doing it under the mutex would mean
	// every woken goroutine immediately contends for a lock it does not need.
	close(c.done)
}

// pendingCount reports how many requests are in flight. Test support, but
// exported to the package rather than the test file because the leak it
// guards against is the whole point of this unit.
func (c *client) pendingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

// DropForTest severs an inbox's tunnel the way a network flap would: no
// close frame, no warning.
//
// Exported because the acceptance test in internal/server needs to simulate
// a drop and cannot reach unexported fields across packages. Named so that
// nobody mistakes it for an operational control -- the real eviction paths
// are register/unregister.
func (h *Hub) DropForTest(endpointID string) {
	h.mu.Lock()
	c := h.clients[endpointID]
	h.mu.Unlock()
	if c != nil {
		_ = c.conn.CloseNow()
	}
}
