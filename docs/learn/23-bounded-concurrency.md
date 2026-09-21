# 23 — Semaphores, bounded concurrency, and backpressure

*Phase 3, unit 7. Covers the curriculum bullet "semaphores and bounded concurrency; what
backpressure means and what happens without it". This is failure mode 9 from `PLAN.md`,
and it closes the gap recorded in [20](20-forwarding.md) and [21](21-the-cli.md).*

## Brief

**What is this thing?** A **semaphore** is a counter that limits how many things may happen
at once. **Bounded concurrency** is deliberately capping in-flight work. **Backpressure**
is what you do with the excess — the signal that travels back to whoever is producing the
work, telling them to slow down or stop.

**What problem does it exist to solve?** Unbounded concurrency looks perfect right up until
load arrives. Every simultaneous request costs something: a goroutine and its stack, an
entry in a map, a socket, memory holding a body. Each is small, and none of them is the
problem. The problem is that nothing multiplies them by a number you did not choose.

The failure is also the worst available shape. A system with no bound accepts all five
hundred requests, becomes slow at all five hundred, and then every one of them times out —
so the work is done badly, nobody is served, and the resources were spent anyway. Doing
thirty-two things well and making the rest wait is strictly better than doing five hundred
things so badly that none finishes.

Worth noticing that this used to be accidental. Threads were expensive, so a thread pool
was mandatory and the bound came for free. Go made goroutines so cheap that "just spawn
one" became the default, which removed a limit nobody had to think about before.

**How does it actually work underneath?** In Go a counting semaphore is a buffered channel
and nothing else. Sending on it is *acquire*, receiving is *release*, and the buffer's
capacity is the limit. When the buffer is full the send blocks — and that blocking **is**
the waiting, with no condition variables or counters to get wrong. Wrap the send in a
`select` with `ctx.Done()` and the wait becomes bounded too.

One level down, onto the part people skip. Bounding the workers is only half a design; you
must also decide what happens to the excess, and there are exactly three answers.
**Wait** — right when the producer is already waiting anyway and a deadline exists to stop
the wait being infinite. **Reject** — right when a queue would grow without bound, because
failing fast is more honest than accepting work you cannot get to. **Drop** — right only
when the data is replaceable, which is what the broker in [14](14-pubsub.md) does, because
a missed event can be recovered by refetching.

And the trap: *a bounded worker count with an unbounded queue is not bounded*. You have
moved the unboundedness from goroutines into the queue and hidden it. Something has to
bound the queue too — a length, or a deadline.

**What are the sharp edges?**

- **Release with `defer`, at the moment of acquiring.** One early return that skips the
  release permanently shrinks the capacity, and enough of them deadlock the system with no
  error anywhere.
- **Never acquire on a path that must stay responsive.** Blocking a connection's read loop
  waiting for a slot stops it processing control frames — pings included — so the peer
  concludes you are dead when you were merely busy.
- **The limit is a decision, not a guess to bury.** It should be derived from what the
  slowest component can actually take.

**In hooklens:** the hub bounds in-flight forwards **per tunnel**, so one busy inbox cannot
consume the process. The CLI separately bounds concurrent requests to the local app,
because a development server is frequently single-threaded and being handed thirty
simultaneous requests is not a kindness. Excess waits, bounded by the deadline already in
play, and a forward that never gets a slot records `overloaded` — distinct from `timeout`,
because "we were too busy" and "your app did not answer" send a developer to different
places.

## Decisions

**Excess waits; it is not rejected and not dropped.**
The three answers from the brief, and why this one. The producer — the ingest handler — is
*already* blocked waiting for the provider, so making it wait a little longer costs nothing
extra. And the wait is bounded by a deadline that already exists, so the queue cannot grow
without limit. Rejecting would fail requests that a few hundred milliseconds would have
served; dropping is only ever right for replaceable data, which a webhook is not.
*Wrong call if* the queue were unbounded, at which point accepting work you cannot get to
is dishonest and fast rejection is kinder. The deadline is what keeps that from being true
here.
*The signal to switch:* `overloaded` appearing regularly rather than never.

**The bound is per tunnel, not per process.**
One inbox being hammered must not starve every other one. A process-wide limit would let a
single misbehaving integration consume the whole service, which is the noisy-neighbour
failure that per-tenant limits exist to prevent.

**32.**
Derived from what the slow end can take, not picked for roundness: the far side is a
development server on somebody's laptop, frequently single-threaded, and handing it
hundreds of simultaneous requests is not a kindness. It is also far above what any real
provider sends to one inbox, so the limit should be invisible until something is genuinely
wrong.

**`ErrOverloaded` is distinct from `ErrTimeout`, and needed a migration to stay distinct.**
Folding them together was the cheap option and would have been the first time this phase
conflated two failures that send a developer to different places. `timeout` says their
handler is slow. `overloaded` says we never asked it. Keeping them apart meant migration
00007 widening the CHECK constraint, which is the right amount of friction for adding a
value to a closed set.

**A buffered channel, not `sync.Mutex` plus a counter, and not `golang.org/x/sync/semaphore`.**
The channel version is four lines, has no condition variable to get wrong, and composes
with `select` so the wait is cancellable for free. The `x/sync` package is fine and would
be a dependency for something the language already does.

**The client acquires INSIDE the per-request goroutine, never in the read loop.**
The most important line in the unit, and it is a line that is *not* there. Acquiring in
the read loop would block it whenever the local app is busy — and a blocked read loop
stops processing control frames, including the pong the server's keepalive is waiting for.
The server would then conclude the CLI is dead when it is merely busy, drop the tunnel, and
turn a slow handler into an outage. Same shape as the broker rule in
[14](14-pubsub.md): the path that must stay responsive never waits on anything.

**Both ends bound, for different reasons.**
The server's limit bounds goroutines, correlation entries and wire traffic. The client's
limit bounds concurrent requests to the local app. The client's is *redundant* against a
well-behaved server — it will never see more than 32 outstanding anyway — and it is there
because the client does not get to assume the peer is well-behaved.

**`callLocalBounded` exists so the release can be deferred.**
A separate three-line method purely so `defer c.sem.release()` sits immediately after the
acquire. Inlining it would have put the release on one branch of an if/else, which is how
a future edit drops it — and a missed release shrinks the limit permanently, silently, and
cumulatively.

**Migration 00007's down step nulls out `overloaded` rows before narrowing the constraint.**
Postgres validates existing rows when a CHECK is added, so a rollback would otherwise fail
on any database that had recorded one. That loses information, which is the honest cost of
going backwards; a down migration that fails on a busy database is worse than a lossy one.

## Walkthrough

### `internal/tunnel/semaphore.go`

The whole file is 60 lines and most of it is comments, which is the right ratio for
something whose correctness is entirely in when you call it.

`acquire` (`:37`) selects on the send and `ctx.Done()`. The comment records the
bounded-queue argument: returning an error rather than blocking forever is what stops the
unboundedness relocating from goroutines into the queue.

`release` (`:61`) carries the defer rule.

### `internal/tunnel/hub.go`

`client.sem` (`:129`) is per client, and `send` (`:143`) acquires before allocating
anything else, with `defer c.sem.release()` on the next line.

The `acquire` failure returns `ErrOverloaded` (`:153`) rather than passing `ctx.Err()`
through, which would have surfaced as a timeout.

### `internal/tunnel/client.go`

The comment above `go c.handle(...)` (`:288`) is the load-bearing one: it explains why the
acquire is *not* on that line.

`callLocalBounded` (`:344`) — the deferred release.

### `migrations/00007_overloaded.sql`

Drop and recreate, both in one migration and therefore one transaction, so there is no
window in which the column is unconstrained.

## Verified

**Six tests in `internal/tunnel`**, four on the semaphore itself and two on the hub.

`TestSemaphoreNeverExceedsLimit` runs 200 goroutines against a limit of 8 and records the
peak with a compare-and-swap loop. It asserts the peak is **not above** the limit and
**not far below** it — the second half matters, because a semaphore that over-restricts
passes the obvious test while quietly halving throughput.

`TestSemaphoreRespectsCancellation` also checks that a cancelled waiter did not take a slot
on its way out.

`TestHubBoundsInFlightPerTunnel` releases 120 forwards at a tunnel whose handler holds
every request open, measures the peak, and then asserts **every one of the 120 still
completes** — bounding is not dropping.

`TestHubReportsOverloadedNotTimeout` fills every slot and checks the 33rd reports
`ErrOverloaded` and specifically **not** `ErrTimeout`.

### Live, and the first attempt did not prove anything

150 concurrent `curl` processes all returned 201, and the local app reported a peak
concurrency of **13** — below the limit of 32. That is not evidence the bound works; it is
evidence 150 Windows processes cannot start fast enough to saturate it. Recorded because
the number looked like a pass and was not one.

Replaced with a single Go process holding 300 goroutines at a starting gate, released
together:

| burst | result | local app peak |
|---|---|---|
| 50 | all 201 | **32** |
| 100 | all 201 | **32** |
| 200 | all 201 | **32** |
| 300 | 231 of 300 | **32** |

Exactly 32 at every size, and up to 200 nothing fails — the excess waits, which is the
design. The database agrees: **983 successful forwards, slowest 2592ms, and zero
`overloaded` rows.** Queueing, not rejection.

### The 300-request failures, diagnosed rather than waved at

At 300, 69 requests failed — and it is worth being precise about where, because "69 of 300
webhooks failed" would be a serious claim about this system.

They failed at **dial time**: `connectex: No connection could be made because the target
machine actively refused it`, before any HTTP was sent. The server log contains no error
and no capture for them, which is consistent: they never arrived. This is the OS accept
backlog overflowing under 300 simultaneous SYNs, and it is a property of the listener, not
of the tunnel.

Two things worth taking from it. It is the **honest** failure — nothing was stored, so a
provider retrying produces no duplicate, exactly as the durability line requires. And it
marks where the next bottleneck lives: tuning the listener and measuring under sustained
load is Phase 5's job, and now there is a number to start from.
