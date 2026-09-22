# 25 — Showing delivery: three states, not two

*Phase 3, unit 9. The last piece of Phase 3's "done when" list: killing the local app must
**show** as unreachable, not merely be recorded as it. Uses the patterns from
[15](15-server-state.md) and [16](16-recursive-rendering.md) rather than introducing new
ones; the idea worth teaching here is about honesty in a status display.*

## Brief

**What is this thing?** The part of the UI that answers "did this webhook actually reach my
app?" — which, now that the tunnel exists, is a different question from "was this webhook
captured?" and has to be answered separately.

**What problem does it exist to solve?** Everything since unit 20 has been recording a
forwarding outcome on every capture, and until now none of it was visible. A value written
to a column that no interface reads is not a feature; it is a comment stored in Postgres.

More specifically, this closes the loop the tool exists to close. A developer's real
question is rarely "what did Stripe send" on its own — it is "Stripe says it delivered, my
app says it saw nothing, which of us is wrong". Answering that requires showing both halves
on the same row: what arrived, and what happened when we tried to hand it on.

**How does it actually work underneath?** The data is already there —
`forward_status`, `forward_error` and `forward_ms`, all nullable — and the rendering is the
list and detail work from units 15 and 16. The only real design question is how to
represent the outcome, and it is a question about **arity**.

The tempting model is binary: delivered or failed. It is wrong, because there are **three**
states and the third is the most common one. A capture can be *delivered* (we asked the app
and it answered), *failed* (we tried and could not), or **not attempted** (no tunnel was
connected — the normal state of an inbox nobody is currently forwarding).

Collapsing three into two is the bug. If "not attempted" renders as a failure, every
inbox used purely for inspection — the original feature, and still a legitimate way to use
this — displays a wall of red for working correctly. If it renders as success, a developer
whose CLI has quietly died sees green while nothing is being delivered. Both are the
interface lying, in opposite directions.

This is why the API omits the fields entirely rather than sending nulls, and why the
database allows null rather than defaulting: *absent*, *present-and-good* and
*present-and-bad* are three distinguishable things at every layer, and each layer that
flattens them makes the layer above guess.

**What are the sharp edges?**

- **A status with no timestamp is a trap.** "unreachable" from an hour ago, shown next to a
  live tunnel, invites exactly the wrong conclusion.
- **Colour alone is not a status.** It fails for the colour-blind and disappears in a
  screenshot pasted into a bug report, which is precisely where these end up.
- **The failure codes are machine strings, not prose.** They need translating once, in the
  UI, rather than being rendered raw — `no_tunnel` is a key, not a sentence.

**In hooklens:** each row carries a small delivery badge, the detail pane gets a delivery
section with the status, the reason in plain words and the round-trip time, and an inbox
with no tunnel says so once at the top rather than on every row.

## Decisions

**A `Delivery` union in its own module, not booleans in the components.**
`deliveryOf` turns three nullable fields into one of three shapes, and the components
switch on `kind`. The alternative — `capture.forward_error ? … : capture.forward_status ? …`
inline — spreads the three-state logic across every place it renders and guarantees the
two places disagree eventually. The union also makes the exhaustiveness a compiler
concern.

**"Not attempted" renders as nothing on the row, and one notice above the list.**
An inbox with no tunnel is not broken; it is the original product. A badge per row would
paint a wall of grey over a system working exactly as intended, and the information is
identical on every line, so it belongs in one place.

**An app 4xx/5xx is styled apart from a delivery failure.**
`d-appfail` and `d-fail` are different classes on purpose. A 500 means the request reached
the handler and the handler said no — the developer's own code, which is frequently what
they are trying to see. Rendering it identically to "the tunnel is down" would send them
to check their network.
The detail pane goes further and says it in words: *that is your app's response, not a
delivery failure.*

**Failure codes are translated once, in one map, and an unknown code still renders.**
A server newer than the bundle is a normal situation during a deploy. Falling back to an
empty label would make it indistinguishable from "not attempted" — the exact collapse this
whole unit exists to prevent — so an unrecognised code renders as itself with a generic
detail.

**Every failure's detail says what to do**, not just what happened. `no_tunnel` names the
command to run; `unreachable` asks whether the app is running; `too_large` names the
environment variable. A status that leaves the reader with nowhere to go is a decoration.

**"No tunnel" is derived from the newest capture, not from a live endpoint.**
There is no "is a tunnel attached" API, and adding one would be a second source of truth
that could disagree with the rows it sits above. The freshest capture is the best evidence
available, and it cannot contradict itself.
*Wrong call if* an inbox is idle for a long time after the CLI connects — the notice would
linger against a working tunnel. Acceptable because the next capture corrects it, and the
alternative is a poll.

**`X-Hooklens-Id` on every capture response.**
Added because the acceptance test could not find the capture id. On a *successful* forward
the body belongs to the developer's app, so there is no hooklens JSON to read — the id had
nowhere to go. It is genuinely useful beyond the test: it is the only way anything
downstream can correlate the response it received with the capture that produced it.

**`CodeReplaced` became a permanent error.**
The biggest decision in this unit, and it came out of a hang rather than a design session.
See below.

## Walkthrough

### `web/src/lib/delivery.ts`

`Delivery` (`:20`) is the three-state union. `FAILURES` (`:25`) is the one place codes
become sentences.

`deliveryOf` (`:58`) checks `forward_status` first — if both somehow appear, a status
wins. The database forbids that combination, and a UI that renders a contradiction as a
blank is worse than one that picks.

`deliveryBadge` (`:88`) returns `null` for `none`, which is what keeps the notice out of
every row.

### `web/src/App.tsx`

`noTunnel` (`:56`) and the notice it drives (`:102`).

`DeliveryBadge` (`:164`) renders nothing rather than something empty.

### `web/src/Detail.tsx`

`Delivery` (`:149`) — three branches, and the middle one distinguishes an app error from a
delivery error in words.

### `internal/server/phase3_test.go`

The four `t.Run` legs are `PLAN.md`'s "done when" list in order, sharing one inbox
deliberately: they are not independent, and each leaves the system in the state the next
one starts from.

## Verified

**Nine frontend tests** for `delivery.ts`, all on the three-state property: no attempt is
`none` and shows no badge, a 500 is `delivered` with `d-appfail` and explicitly **not**
`d-fail`, every known code has a translated label and a detail over 20 characters that
does not fall through to the unknown-code text, and an invented code still renders as a
failure.

**An automated acceptance test** — `TestPhase3DoneWhen` — running `PLAN.md`'s four criteria
against a **real `tunnel.Client`**, a real local app, and real Postgres, in one run:

| leg | asserts |
|---|---|
| three-terminal | the app's 201, its header and its echoed body reach the sender; the app was hit once; `201` recorded |
| dead local app | 200 to the sender, `unreachable` recorded and readable |
| tunnel killed mid-request | ingest unblocks in well under the 20s failure bound |
| URL survives a drop | same URL back by itself, **and forwarding works again** |

Total runtime under a second. The manual three-terminal demo is the right way to show
this and the wrong way to keep it working; this is the version that runs on every commit.

Frontend bundle 267 KB raw, **83 KB gzipped**.

### The livelock this found

The acceptance test hung — not failed, hung, for the full five-minute timeout. The cause
was a genuine design gap that no unit test would have found, because it needs **two real
CLI processes**.

Failure mode 5 says newest wins and the displaced client is told why. Unit 22 then
classified `replaced` as *transient*, since it is not the client being wrong. Put those
together and two CLIs on one inbox **fight forever**: each is replaced, each immediately
reconnects, each evicts the other, neither ever delivers reliably. From the outside it
looks like a flapping tunnel; in fact both machines are busy stealing an inbox from each
other.

`CodeReplaced` is now permanent. A displaced client exits and says so, which is right:
the user started a second client deliberately, and the first should get out of the way.

Worth being precise about what this does *not* change. The network-flap case — the reason
reconnection exists at all — has only one CLI process, and the connection it displaces on
reconnect is a dead socket with nobody reading it. There is no fight because there is no
second reader. The two cases look identical at the protocol level and are completely
different in practice, which is why the classification had to be made deliberately rather
than by pattern-matching on "is this the client's fault".

---

## Postscript — the live view never learned the outcome

Found in Phase 6, recording the README demo with a real tunnel attached: a webhook
was delivered to the local app — `X-Hooklens-Forward: delivered`, and the database
held `forward_status 200, forward_ms 90` — yet the open browser showed the capture as
*not attempted*, under the notice telling the user to start the tunnel they were
already running. A reload showed the truth.

**The cause is an ordering this unit never examined.** The `capture` stream event is
published the moment the request is stored, deliberately *before* forwarding, so the
row appears in milliseconds instead of after a round trip to a laptop. Nothing ever
published the outcome afterwards. Every test of this unit rendered captures fetched
from the API, which carry the outcome — none watched a capture arrive live and then
get delivered, which is the only path that shows it.

**The fix is a second, small event.** `ingest.publishDelivery`
(`internal/ingest/ingest.go`) sends `delivery` with `{id, forward_status |
forward_error, forward_ms}` once `RecordForward` has succeeded — only then, so an open
browser never shows a state a reload would contradict. The client patches the row and
any open detail pane in place (`applyDelivery` / `withDelivery` in
`web/src/lib/delivery.ts`), replacing the three outcome fields *as a set*, because a
stale `forward_error` left beside a new `forward_status` would make `deliveryOf` report
a failure for a delivered capture.

**Rejected:** publishing the capture event after the forward. One event instead of
two, and it makes the list only as live as the slowest local handler — up to 30
seconds of a webhook that has arrived but is not on screen.

**Tests:** three in `internal/ingest/delivery_event_test.go` (success, failure, no
subscribers — including that absent fields are omitted, not null) and four in
`web/src/lib/delivery.test.ts` (marks delivered, no-op for an unlisted row, replaces
a stale error, does not mutate).
