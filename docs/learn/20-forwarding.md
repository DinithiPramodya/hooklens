# 20 — Forwarding, and what the provider is told

*Phase 3, unit 4. Covers failure modes 1, 2 and 3 from `PLAN.md` end to end: the captured
request is actually handed to the tunnel, and the provider gets an answer that is honest
about what happened without punishing the developer for a laptop that is asleep.*

## Brief

**What is this thing?** The step that turns an inspector into a tunnel. A webhook has
arrived and been stored; now it has to be handed to the hub, sent down the CLI's
connection, delivered to the app on the developer's laptop, and its response brought back.

**What problem does it exist to solve?** Storing a webhook lets you *look* at it. It does
not let you *develop against* it — for that, the request has to reach the code being
written, and the code's answer has to reach the provider, because a provider that never
sees a 2xx will retry, back off, and eventually disable the endpoint.

**How does it actually work underneath?** The interesting part is not the plumbing, which
is one call into the hub from unit 19. It is **where** the call goes and **what the
provider is told**.

The placement is fixed by something already established: the durability line. Everything
after the row is committed must be incapable of turning a stored request into a non-2xx,
because a provider reads any non-2xx as "not delivered" and sends the same event again.
Forwarding therefore happens *below* that line, and no forwarding failure may become an
error status. That single constraint decides most of the design.

Then the real decision: forwarding is **synchronous**. The ingest handler blocks while the
request goes to a laptop and comes back, so it can relay the actual response. The
alternative — answer the provider at once and forward in the background — is tempting and
loses the thing the feature exists for: the provider would receive our invented 200 rather
than the application's real answer, so a developer could never test what their handler
actually returns, nor see a signature rejection, nor exercise retry behaviour. The cost is
that the provider's latency now includes the developer's laptop, and that cost is real.

Finally, what the provider sees when forwarding *fails*. There are two different kinds of
failure and conflating them is the trap. **The local app was reached and answered badly**
— a 500 from the handler — is a real answer and gets relayed unchanged; the developer is
testing exactly that. **The local app was never reached at all** — no tunnel, connection
refused, timeout — is not the provider's business. Returning 502 there would make the
provider retry, back off, and possibly disable the endpoint, all because someone closed
their laptop lid. So a delivery failure returns the endpoint's configured response, with a
header explaining what happened, and the failure is recorded on the capture for the UI.

**What are the sharp edges?**

- **Synchronous forwarding couples two latencies.** A slow local handler is now a slow
  webhook response, and if it exceeds the provider's own timeout the provider retries —
  producing a second capture and a second forward of the same event.
- **A deadline is not optional.** Without one, a hung local app holds an ingest goroutine,
  a correlation entry, and the provider's connection indefinitely.
- **"No tunnel" is a normal state, not an error.** Most inboxes have no CLI attached most
  of the time; that path has to be as clean as the connected one.
- **Concurrency is still unbounded here.** Five hundred simultaneous webhooks means five
  hundred blocked goroutines and five hundred correlation entries. That is failure mode 9
  and it is deliberately not solved in this unit.

**In hooklens:** ingest calls `hub.Forward` below the durability line with a 30-second
deadline, relays the response when one comes back, and otherwise answers with the capture's
ordinary `200` plus an `X-Hooklens-Forward` header naming what went wrong.

## Decisions

**Forwarding is synchronous, inside the ingest handler.**
The handler blocks while the request travels to a laptop and back, so it can relay the
real response. Asynchronous forwarding — answer the provider immediately, deliver in the
background — is cheaper and loses the feature: the provider would get our invented 200
instead of the application's answer, so a developer could never test what their handler
returns, never see a signature rejection reach the provider, never exercise retry
behaviour.
*The cost is real and worth stating:* the provider's latency now includes the developer's
laptop, and if a local handler is slower than the provider's own timeout, the provider
hangs up and retries — producing a second capture of the same event.
*Wrong call if* hooklens were a delivery service rather than a development tool. For a
production relay, accept-then-deliver with retries is correct.

**Below the durability line, and structurally incapable of breaking it.**
`forward` returns a decision, never an `error`. That is not stylistic: an error return
invites a caller to turn it into a non-2xx, and a non-2xx on a stored capture is the
Phase 0 quiz Q1 bug — the provider retries an event we already hold. Making the failure
path unrepresentable is stronger than remembering not to take it.

**A delivery failure and an application failure are different things.**
The distinction the whole unit turns on.
*Reached and answered badly* — a 500 from the handler — is a real answer, relayed
unchanged, because testing exactly that is why the developer is here.
*Never reached* — no tunnel, connection refused, timeout — is not the provider's business.
Returning 502 there makes the provider retry, back off, and eventually disable the
endpoint, all because somebody closed their laptop. So it returns the capture's normal 200
with an `X-Hooklens-Forward` header naming the reason.
This is why `tunnel.Response` carries `Error` separately from `Status`: collapsing them
would tell a developer their application is broken when in fact it is not running.

**The forward runs on a context detached from the request's.**
`context.WithoutCancel(ctx)` plus our own 30s deadline. `r.Context()` is cancelled the
moment the provider hangs up — and a provider whose timeout is shorter than ours does
exactly that. Tied to it, the developer's app would have its request cancelled mid-handler,
which looks like their own code misbehaving. Finishing the delivery and recording the
outcome is the honest thing even when nobody is left to receive the response.
The same reasoning applies to `RecordForward`, which also gets a detached context: the
recorded outcome is the only durable evidence of what happened, and losing it precisely
when a forward went slowly would hide the cases most worth seeing.

**30 seconds.**
Long enough for a debugger paused on a breakpoint to be a little useful, short enough to
sit near the shortest common provider timeouts, so in the normal failure case *we* give up
first and answer deliberately rather than being hung up on mid-forward.

**Hop-by-hop headers are stripped from the relayed response.**
`Connection`, `Keep-Alive`, `Transfer-Encoding`, `Upgrade`, and friends describe the CLI's
connection to the local app, not ours to the provider. `Content-Length` is stripped too,
for a subtler reason: it describes a different response than the one `net/http` is about
to write, and a mismatched one produces a truncated or hanging body.

**An out-of-range status becomes 502 rather than a panic.**
`WriteHeader` panics on a code outside 100–599, so a buggy CLI could take down the capture
endpoint for everyone. Clamping costs two lines.

**`forwardErrorCode` has a `default` arm that returns a permitted value.**
The codes are fixed by a CHECK constraint in migration 00006. An unmapped error falling
through to its own text would violate that constraint on the UPDATE — and surface as a
database fault logged far from its real cause, which is a new error type added upstream in
`internal/tunnel`. There is a test that walks every hub error and asserts the result is one
the constraint accepts.

**`Forwarder` is an interface declared in `internal/ingest`.**
The mirror image of `AuthFunc` in `internal/tunnel`: the consumer declares the narrow
contract it needs. Ingest does not learn that tunnels are WebSockets, the dependency stays
one-way, and the unit tests run with no sockets at all.

**`nil` is a supported value for it.**
hooklens is a complete inspector with no tunnel feature, and that path has to stay clean
rather than being a special case bolted on. `forward` returns a zero outcome and nothing is
recorded.

**The tunnel is constructed before ingest in `server.New`.**
Trivial-looking and load-bearing: ingest forwards through the hub, so building ingest first
with `nil` and intending to fill the field in later is how a handler ends up silently never
forwarding. The ordering makes the dependency a compile-time fact.

**Migration 00006: nullable columns, and a CHECK that the two are exclusive.**
Null means "not attempted", not "failed" — a meaningful distinction, because every row is
briefly null (inserted before forwarding, as the durability line requires) and a row that
*stays* null is one where the process died mid-request. A `NOT NULL DEFAULT` would have
erased that signal.
`forward_status` and `forward_error` are mutually exclusive by constraint rather than by
convention, so a handler bug is caught at the write instead of rendered as a contradiction.
No index: nothing filters on these, and the same argument as 00005 applies — every capture
would pay to maintain an index only a future analytics query would use.

**`RecordForward` is an UPDATE, which bends the append-only rule from 00002.**
Acknowledged rather than glossed: the row must exist before forwarding is attempted, so the
outcome can only be written afterwards. One UPDATE of three columns on a row that is still
in cache.

## Walkthrough

### `migrations/00006_forwarding.sql`

Three columns and one constraint. The comment on `forward_error` explains why it is a short
code and not free text: the UI switches on it, and a message written for humans changes
wording and silently breaks that.

### `internal/ingest/ingest.go`

`Forwarder` (`:277`) — the consumer-declared interface.

`forward` (`:301`) is the whole attempt. Note what it returns: a `forwardOutcome`, never an
error. The detached context at `:315` and the reason for it.

The `resp.Error != ""` branch (`:338`) is where a delivery failure is separated from an
application failure. Everything downstream depends on that split being made here.

`forwardErrorCode` (`:358`) — the `default` arm and the constraint it protects.

`writeForwarded` (`:383`) relays status, headers and body. `Header().Add`, not `Set`, so
duplicate headers survive the last hop the way they survived every earlier one.

The capture handler's tail (`:186`–`:215`) is the ordering: forward, record, then either
relay or fall back. `RecordForward` runs before the response is written so that a slow
database cannot leave the outcome unrecorded after the provider has already been answered.

### `internal/server/server.go`

`New` (`:75`–`:93`) — the tunnel before ingest, and the comment saying why.

`Server.ingest` is now `*ingest.Handler` rather than `http.Handler` (`:39`). The server
configures it, and hiding that behind the interface would mean a type assertion to reach
its own dependency.

## Verified

**Five unit tests in `internal/ingest`** with no database and no sockets, covering the
pure decisions: every hub error maps to a code the CHECK constraint accepts, hop-by-hop
headers are recognised case-insensitively, a relayed response keeps duplicate `Set-Cookie`
headers and drops `Connection` and `Content-Length`, a nonsense status becomes 502 instead
of a panic, and a malformed base64 body still relays its status.

**Five end-to-end tests in `internal/server`** running the entire path: an HTTP POST to the
capture endpoint, a row in Postgres, a frame down a real WebSocket, a fake CLI's answer,
and the response the provider actually receives.

| test | asserts |
|---|---|
| `TestForwardEndToEnd` | the local app's **418**, its `X-Brewed-By` header and its body reach the provider unchanged — and the app saw the right method, path (inbox prefix stripped), query and body |
| `TestForwardWithNoTunnel` | 200, `X-Hooklens-Forward: no_tunnel`, `forward_error` recorded, `forward_status` **nil** |
| `TestForwardUnreachableLocalApp` | 200 and `unreachable` — **not** a 502 |
| `TestForwardRelaysApplicationErrors` | a 500 *from the app* is relayed verbatim, body and all |
| `TestForwardTimeoutStillCapturesAndRecords` | the provider is released at the deadline with a 200, and `timeout` is on the row |

The middle three are the unit's argument in executable form: the same "delivery failed"
outcome that must **not** become a 502, next to an application 500 that **must** be
relayed.

Migration 00006 applied and reversed cleanly against real Postgres — up, down, up.
Everything green under `-race`.

### Known gap, deliberately

Concurrency here is still unbounded. Five hundred simultaneous webhooks means five hundred
blocked ingest goroutines and five hundred correlation entries. That is failure mode 9, it
is not solved in this unit, and it is written down here rather than discovered later.
