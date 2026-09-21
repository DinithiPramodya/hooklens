# 24 — Size limits at every boundary

*Phase 3, unit 8. Failure mode 8 from `PLAN.md`, taken end to end rather than only at
ingest. Builds on [06](06-reading-a-request.md), which capped the first boundary; this
note is about the four others that appeared once the tunnel existed.*

## Brief

**What is this thing?** A size limit is a decision about what to do when something is
bigger than you planned for. There is one at every point where bytes cross a boundary, and
a system is only as honest as its least honest one.

**What problem does it exist to solve?** The obvious problem is memory: an unbounded read
lets a sender decide how much you allocate, which is the vulnerability from unit 06. That
one is well understood and easy to fix.

The problem this note is about is subtler and worse. Once you have a limit, **you have to
decide what happens at it** — and the default answer, silently keeping the first N bytes,
is usually the wrong one. A truncated body is not a smaller version of the payload; it is
a *different, corrupt* payload that looks fine. Hand one to a JSON parser and it fails in a
way that blames the sender. Hand one to a signature verifier and it fails in a way that
blames the secret. The person debugging has no reason to suspect truncation, because
nothing said truncation happened.

**How does it actually work underneath?** Once a request crosses several components, each
one has a limit, and they interact in ways that are not obvious from any single file.

In hooklens a body crosses five boundaries: the provider's request into ingest; the
captured bytes into Postgres; the request frame down the WebSocket; the local app's
response back up it; and the relayed response out to the provider. Each of those has a
ceiling, and the ceilings must be **consistent with each other** — because they do not
measure the same thing.

That is the trap worth internalising. A body is stored as raw bytes but travels as
base64, which is **four bytes for every three**: a 2 MB body is a 2.79 MB frame. If the
frame limit equals the body limit, a body at the limit produces a frame over it — and a
WebSocket read-limit violation does not skip a message, it **closes the connection**. So
one large response takes down the whole tunnel, including every unrelated request in
flight on it, and the error names the read limit rather than the response that caused it.

**What are the sharp edges?**

- **Truncation must be loud.** Every truncated thing needs a flag that travels with it, and
  anything downstream that cannot tolerate a partial body must refuse rather than guess.
- **Limits at different layers measure different things.** Raw bytes, base64 bytes, the
  whole frame including JSON. Comparing them without converting is how the tunnel above
  dies.
- **A limit that cannot be raised is a bug for somebody.** Payload sizes are not ours to
  decide; some providers legitimately send megabytes.
- **The error must name the thing, not the mechanism.** "Read limit exceeded" sends
  someone into the WebSocket library. "The response was 4 MB; the limit is 2 MB" does not.

**In hooklens:** the capture limit becomes configurable, a truncated capture is **not
forwarded** — a local handler receiving silently corrupt bytes is worse than one receiving
nothing, and the capture is still stored and inspectable — and the frame limit is derived
from the body limit with the base64 expansion applied, rather than being the same number.

## Decisions

**A truncated capture is not forwarded.**
The decision this unit turns on, and the fork was real. *Forward what we kept* is what a
proxy naturally does and delivers a corrupt payload that looks complete — the handler's
JSON parse fails blaming the sender, or a signature check fails blaming the secret, and
nothing in either message suggests truncation. *Refuse* costs exactly one case, a handler
that tolerates partial bodies, and is better for every other.
Nothing is lost for inspection: the capture is stored, flagged, and visible in the UI,
which is what makes refusing cheap. The escape hatch is `HOOKLENS_MAX_BODY`.
*Wrong call if* hooklens were a delivery service rather than a debugging tool — a relay
should arguably stream rather than cap at all.

**The frame limit is derived from the body limit, not equal to it.**
`maxFrameBytes = maxBodyBytes/3*4 + 256KB`. The two are different measurements and
treating them as one number was a live bug: the client read up to the *frame* limit of
2 MB, base64 made it 2.79 MB, and the server's 2 MB read limit then **closed the
connection** — dropping the tunnel and every unrelated request in flight on it, reporting
a read limit rather than the response that caused it. A test now asserts the inequality
directly, because the next person to tune one of these constants will not re-derive it.

**An oversized local response is refused, not truncated.**
Symmetrical with the capture decision, in the other direction. The client reads
`maxBodyBytes+1` — the probe trick from [06](06-reading-a-request.md) — and returns an
`Error` naming the size, so the provider gets the configured fallback and the developer
gets a message about their response rather than a mystery.

**`MaxHeaderBytes` is set explicitly to 64 KB.**
Go defaults to 1 MB, which is fine for an HTTP server and not fine here: headers travel
inside a tunnel frame next to a base64 body, so the frame limit has to cover both. A
megabyte of headers would exceed it and close the connection. 64 KB is far more than any
real webhook sends, and it makes the frame arithmetic something that can actually be
checked.

**`maxBodyBytes` is a constant in `internal/tunnel`, equal to `capture.DefaultMaxBody`,
and not imported from it.**
The package is deliberately free of the capture package. The cost is that the two agreeing
is a fact rather than a compiler guarantee — so there is a test asserting it, which is the
honest way to hold an invariant a type system is not holding for you.

**`HOOKLENS_MAX_BODY` accepts `1MB`, not just `1048576`.**
A config value people get wrong by a factor of 1024 is a config value that will be got
wrong.

**A malformed size falls back to the default rather than refusing to start.**
Deliberately inconsistent with `HOOKLENS_ENV`, which `Load` rejects outright, and the
inconsistency is the point: there is a safe default here, and the blast radius of a typo
is "captures are 1 MB" rather than "the routing is wrong".

## Walkthrough

### `internal/tunnel/server.go`

The constant block (`:36`–`:61`) is now three limits that measure three different things,
each with the reasoning inline. `maxFrameBytes` carries the description of the bug it
fixes, because the tempting edit is to make them equal again.

### `internal/tunnel/client.go`

The response read (`:417`) uses `maxBodyBytes+1` and checks for overflow — refuse, not
truncate.

### `internal/ingest/ingest.go`

The `req.Truncated` guard (`:317`) sits at the top of `forward`, before the context is
even built, so a truncated capture costs nothing beyond the check.

### `internal/config/config.go`

`envBytes` and the fallback decision above it.

### `cmd/hooklens/main.go`

`MaxHeaderBytes` on the `http.Server`, with the reason it is not left at Go's default.

## Verified

**Four limit tests plus three config tests**, and two of them exist to hold facts the
compiler cannot:

- `TestFrameLimitExceedsEncodedBodyLimit` asserts the inequality that was wrong, with
  headroom for headers on top.
- `TestBodyLimitMatchesCaptureLimit` asserts `maxBodyBytes == capture.DefaultMaxBody`,
  since the packages are deliberately not coupled.
- `TestCallLocalRefusesOversizedResponse` — an `Error`, **status 0**, and no partial body.
- `TestCallLocalAcceptsResponseAtTheLimit` — the boundary itself works, and the frame it
  produces fits the read limit. That last assertion is the one that would have caught the
  original bug.

**End to end**, `TestTruncatedCaptureIsNotForwarded` drops the cap to 64 bytes, posts 4 KB,
and checks all four consequences at once: the provider gets 200 (the capture is stored),
the header says `too_large`, **the fake CLI is never reached**, and the row holds 64 bytes
with `body_truncated` set. Storing what fits is what makes not forwarding acceptable, so
the test asserts both halves.

Migration 00008 up, down and up against real Postgres.
