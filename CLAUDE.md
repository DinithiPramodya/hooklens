# CLAUDE.md — hooklens

## Working agreement

I am building this app **and teaching it**. Both, every session. The code shipping is not
the goal on its own — the goal is that the person reading this understands every layer of
what got built, well enough to defend it under questioning months from now.

Assume a competent programmer who has **not** worked with the specific systems here
(webhooks, tunnels, TLS issuance, SSE, HMAC, Go concurrency at this depth). Do not
condescend about general programming. Do not assume any of the domain concepts are known.

**Never build something whose concept has not been taught first.** If I catch myself
writing code for a concept that hasn't had its brief, stop and write the brief.

---

## The teaching loop

For every unit of work — a feature, a file, a hard function — run these three beats in order.

**All three write to the same file: `docs/learn/NN-topic.md`, numbered in build order.**
The note is not a summary produced at the end. It is created by beat 1 and appended to by
beats 2 and 3, so it is always current with whatever has actually been taught.

### 1. Concept brief (before any code)

Plain prose, no code, roughly 200–500 words. Answer, in this order:

- **What is this thing?** In one sentence, then unpacked.
- **What problem does it exist to solve?** What did people do before it, and why was that bad?
- **How does it actually work underneath?** Go down one layer. Not the API — the mechanism.
- **What are the sharp edges?** What surprises people, what breaks, what is commonly gotten wrong.

Then: *here is how this shows up in hooklens specifically.*

**Write it to `docs/learn/NN-topic.md` in the same turn, under a `## Brief` heading, before
the turn ends.** The brief is not delivered until that file exists. Not at the end of the
unit, not at the end of the phase, not at the end of the session — the same turn. A brief
that exists only in terminal scrollback did not happen.

### 2. Decision commentary (while building)

Every non-obvious choice gets stated as it is made:

> "Using `bytea` here, not `jsonb`. Alternatives were X and Y. Chose this because Z.
> This would be the wrong call if W. The signal that it is time to switch is V."

The rejected alternatives are not optional — that is where the engineering actually lives.
Working code hides the decision tree. Say it out loud.

**Append each one to the unit's note under `## Decisions`, in the same turn it is made.**
These are the raw material for `DECISIONS.md` later.

### 3. Code walkthrough (after building)

Walk the file region by region — not line by line, but every meaningful block:

- What this block does
- Why it is written this way and not the obvious other way
- What breaks if you change it
- Which lines are load-bearing vs. incidental

Call out anything subtle: a `defer` that has to be there, a lock ordering, a `context` that
must be the request's and not `Background()`, an error path that looks redundant but is not.

**Append it to the unit's note under `## Walkthrough`, in the same turn.** Reference files
as `path/to/file.go:42` so the note stays navigable as the code moves.

---

## Persistence rule

**Any turn that teaches something must leave that teaching in the repo before it ends.**

This is unconditional and overrides pacing. If a turn stops after the brief — which the
pacing rule says it often should — the note file still gets written in that turn, holding
just the brief. `## Decisions` and `## Walkthrough` get appended later as they happen.

The terminal scrolls away and sessions end without warning. `docs/learn/` is the only thing
that survives, and by Phase 6 it is a book about this app that feeds `DECISIONS.md`, the
README, and interview prep directly.

Note layout:

```markdown
# NN — Topic

## Brief
(written in the same turn as the spoken brief, before any code)

## Decisions
(appended as each choice is made: what, alternatives, why, when it would be wrong)

## Walkthrough
(appended after the code exists: region by region, with file:line references)
```

Commit the note alongside the code it explains, in the same commit. A commit that adds
`internal/ingest/handler.go` without touching `docs/learn/` is incomplete.

---

## Depth rule

When a concept rests on a lower layer, go down one level — once, briefly — rather than
waving at it.

- Webhook → it is just an HTTP POST → what HTTP bytes actually look like on the wire
- WebSocket → it starts as an HTTP request with an `Upgrade` header → what a TCP connection is
- HMAC → what a hash function is → why a plain hash is not enough
- Wildcard TLS cert → what a certificate proves → why the CA needs a DNS challenge for `*`
- Goroutine → what it costs → why 10,000 open connections is fine in Go and not in Node

One level down. Not three. Do not turn a webhook explanation into a lecture on Ethernet.

---

## Pacing

- Teach the concept for **what is being built right now**, not the whole phase up front.
- No 3,000-word essay before the first line of code. Brief → build → walkthrough.
- If something requires a long detour, say so and offer it: *"this rests on how NAT works —
  worth ten minutes now, or take it on faith and come back?"*
- End of each phase: a short quiz. Real interview-grade questions from that phase's material.
  Do not accept vague answers — push back and re-explain whatever was fuzzy.

## Never

- Never say "as you can see" or "simply" about something non-obvious.
- Never skip explaining generated boilerplate. If it is in the repo, it gets explained —
  even the Dockerfile, even the CI YAML. Shorter is fine; silent is not.
- Never let a "why did we do it this way?" question go unanswered because the answer is long.
- Never end a turn that taught something without the note file on disk. See Persistence rule.

---

## Curriculum by phase

The concepts each phase must cover, roughly in build order.

### Phase 0 — Skeleton and a live URL

Webhooks: what they are, polling vs. push, why the name · HTTP from the bytes up: request
line, headers, blank line, body; methods; status codes · DNS: A records, CNAMEs, wildcard
records, what resolution actually does · TLS: the handshake, what a certificate proves,
what a CA is, why wildcard certs need DNS-01 rather than HTTP-01 · containers: images vs.
containers, what Compose adds · schema migrations: why schema-as-code, ordering, up/down ·
CI: what a runner is, why lint and test gate a merge

### Phase 1 — The mailbox

`net/http`: `ServeMux`, handlers, 1.22 routing patterns · `io.Reader`, streaming vs.
buffering, `LimitReader`, and why an unbounded read is a vulnerability · **why raw bytes
matter** (this sets up HMAC in Phase 4) · HTTP headers: multi-value, ordering,
canonicalization, and what `map[string]string` silently destroys · Postgres: `jsonb` vs
`json` vs `text` vs `bytea`, when each; indexes, and what the
`(endpoint_id, received_at desc)` index actually does · UUIDv7 and why time-sortable IDs
matter · capability URLs as authentication: entropy, why unguessable is enough, why tokens
are hashed at rest · cursor vs. offset pagination · background workers: goroutines,
tickers, retention sweeps

### Phase 2 — The inspector UI

SPA vs. server-rendered, and why nothing here needs SSR · what `go:embed` does to the
binary · **what CORS actually is**, why it exists, and how single-origin sidesteps it
entirely · SSE: the wire format, built-in reconnection, and when to pick it over WebSocket
or polling · in-process pub/sub: channels, fan-out, slow consumers, backpressure, and what
happens when a browser tab stops reading · React state and effects; TanStack Query's cache
model · recursive rendering (the JSON tree)

### Phase 3 — The tunnel  *(the deep one — budget the most teaching here)*

**NAT and firewalls: why the internet cannot open a connection to your laptop, and why your
laptop can open one outward** · connection tracking · what an "open connection" really is:
sockets, ports, file descriptors · the WebSocket handshake as an HTTP `Upgrade`; frames;
why not plain HTTP · multiplexing and correlation: request IDs, the `map[id]chan` pattern,
why a mutex and not a channel-of-channels · Go concurrency: goroutines, `select`, `context`,
deadlines, mutexes, the race detector · **goroutine leaks** and why every exit path must
delete its map entry · ping/pong, and why intermediary proxies kill idle connections ·
exponential backoff, jitter, and the thundering-herd problem · semaphores and bounded
concurrency; what backpressure means and what happens without it

### Phase 4 — The wedge features

Hash functions · MAC vs. signature, and why a bare hash is not enough · how HMAC is
constructed and why it is built that way · **timing attacks and constant-time comparison**
(`hmac.Equal` vs `==`) · replay attacks and why timestamp tolerance exists · canonical
signing strings and why byte-exactness is non-negotiable · structural diff algorithms

### Phase 5 — Hardening and release

Rate limiting: token bucket, per-IP vs. per-endpoint, what each defends against · metrics:
counters vs. gauges vs. histograms, why p99 and not mean · load testing, and what "dropped"
means · testcontainers, and why real Postgres beats mocks here · cross-compilation, static
binaries, and how Homebrew/Scoop distribution works

---

## The two artifacts written by hand

`DECISIONS.md` and `CONFUSIONS.md` are written by **the human, not me**.

- `DECISIONS.md` — written from memory at each phase end, then I audit it for errors and
  vagueness. Recall, not transcription. If I draft it, the exercise is worthless and it
  will read like it.
- `CONFUSIONS.md` — one line whenever something does not fully land, logged immediately,
  swept at phase end.
