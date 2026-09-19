# 06 — Reading a request body, and why the raw bytes matter

*Phase 1, unit 1. Covers the curriculum bullets "`io.Reader`, streaming vs. buffering,
`LimitReader`, and why an unbounded read is a vulnerability" and "**why raw bytes matter**
(this sets up HMAC in Phase 4)", plus the header handling established in
[01](01-webhooks-and-http.md) and tested in [the Phase 0 quiz](../QUIZ.md#q2).*

## Brief

**What is it?** `io.Reader` is Go's one-method interface for "a source of bytes you pull
from": `Read(p []byte) (n int, err error)`. A request body is one. So is a file, a network
socket, a decompressor, an encrypted stream.

The thing to internalise is that a body is **a stream, not a value**. When your handler
starts, the bytes have not all arrived. `r.Body` is a pipe still connected to a machine
somewhere that may be slow, may be malicious, and may never finish. Reading is a
conversation over time, and you are the one who decides when it ends.

**What problem does the interface solve?** Before this style of abstraction you wrote code
against concrete types — a file handle, a socket — and every combination needed its own
plumbing. `io.Reader` has exactly one method, so anything that produces bytes satisfies it,
and anything that consumes bytes accepts it. That is why `io.LimitReader`,
`gzip.NewReader`, `io.TeeReader` and `bufio.Reader` all compose: each takes a Reader and
returns a Reader. You wrap behaviour around a stream rather than reimplementing the stream.

**How does it work underneath?** `Read` fills as much of your buffer as it can *right now*
and tells you how many bytes it wrote. It is permitted to return fewer bytes than you asked
for — a short read is not an error, it just means that is what had arrived. You loop until
`io.EOF`. `io.ReadAll` is that loop, growing a slice as it goes.

Which is exactly where the danger lives. `io.ReadAll(r.Body)` loops until the sender stops
sending. If the sender never stops, the loop never stops, and the slice grows until the
process dies. **An unbounded read from an untrusted client is not a bug, it is a
denial-of-service primitive** — one request, a few kilobytes per second, no memory limit on
your side. You do not need a botnet; you need `curl` and patience. This is why the Phase 0
capture handler deliberately did not read the body: writing the naive version first would
have meant shipping it.

The fix is to wrap the stream rather than trust it. `io.LimitReader(r.Body, n)` returns a
Reader that reports EOF after `n` bytes no matter what the sender does. Memory is now
bounded by a number you chose.

**Why the raw bytes matter.** A body arrives as bytes and you *choose* to interpret them. It
is tempting to parse JSON on the way in and store the parsed object — tidier, and queryable.
Doing that destroys information: key order, whitespace, duplicate keys, number formatting,
and any byte sequence that is not valid JSON at all. In Phase 4 a signature is computed over
the exact bytes the provider sent. Parse and re-serialise and every HMAC check fails, for
reasons that look insane at the time. So the rule for this project is: **store the bytes,
derive everything else.**

**Sharp edges.**

- **`LimitReader` truncates silently.** It reports EOF at the limit, indistinguishable from
  the sender finishing. To know whether you truncated you must ask for `limit+1` and check
  whether you got it.
- **`Content-Length` is a claim, not a fact.** It is a number the sender wrote. Never
  allocate based on it, and never assume the body matches it — chunked encoding omits it
  entirely.
- **`http.MaxBytesReader` is the other tool and does a different job.** It *errors* on
  oversize and signals the server to close cleanly. That is for rejecting input.
  `LimitReader` truncates and keeps going. Which you want depends on whether an oversized
  request is a failure or a thing to record.
- **Not draining the body has consequences.** HTTP/1.1 keep-alive reuses a connection, which
  requires the previous body to be fully consumed. Leave bytes unread and Go closes the
  connection instead — correct, but it silently costs connection reuse.

**In hooklens.** The capture handler is the most hostile-input-facing code in the project:
any method, any path, any body, from anyone who learns the URL. It reads through a
`LimitReader` with the `+1` probe, keeps the raw bytes untouched, and records whether it
truncated so the UI can say so rather than quietly lying.

## Decisions

**Truncate and keep, rather than reject.**
`io.LimitReader` truncates; `http.MaxBytesReader` errors and tells the server to close. Most
servers want the second — an oversized request is an attack or a bug, refuse it. hooklens
wants the first, because "a 5 MB body arrived at your endpoint" is *exactly* the kind of
thing a user opens an inspector to discover. Rejecting it would mean the one request they
most need to see is the one we refuse to record.
*Wrong call if* we were a normal API. *The signal to revisit:* if truncated captures become
common enough that the stored prefix is useless, the limit is wrong, not the strategy.

**The `+1` probe rather than comparing against `Content-Length`.**
`LimitReader` reports `io.EOF` at its limit, which is byte-for-byte indistinguishable from
the sender finishing. Asking for `maxBody+1` makes the two cases distinguishable: receiving
`maxBody+1` bytes proves there was more.
The obvious alternative — check whether `Content-Length > maxBody` — fails on chunked
encoding, which omits the header entirely, and fails on any sender that lies. Verified live:
a chunked request reports `declared_size: -1` and is still read correctly.

**1 MiB body cap.**
Stripe events run a few kilobytes; the largest GitHub push payloads reach tens of kilobytes.
1 MiB is well clear of real traffic and small enough that several hundred concurrent
captures cannot exhaust memory. Configurable per handler, so a future per-endpoint override
is a field, not a refactor.

**Do not drain the remainder of an oversized body.**
Draining would restore HTTP keep-alive on that connection. It would also mean reading an
unbounded number of bytes from a sender we have *just established* is sending more than we
allow — which is precisely the denial of service the limit exists to prevent. Losing
connection reuse on oversized requests is the cheaper side of that trade. Pinned by
`TestReadBodyDoesNotDrain`, which asserts exactly one byte past the limit was consumed.

**A body read error is logged, not returned as a failure.**
A client hanging up mid-body is their doing, not ours, and the bytes that did arrive are
still the answer the user came for. The handler logs a warning and records the partial.
Returning 500 here would also trigger the retry storm from Phase 0 quiz Q1.

**Headers as `[]Header`, and honest about what that preserves.**
Preserves multi-value, which `map[string]string` cannot. Does **not** preserve wire order or
original casing, because `net/http` canonicalises names and merges differently-cased
duplicates before the handler runs, and `r.Header` is a map. Sorting by name is not fidelity
— it is the best available substitute, chosen because determinism is what makes the Phase 4
diff meaningful and the tests stable.
*The rejected alternative* was teeing raw header bytes off the connection with a wrapping
`net.Listener`, recovering the real wire bytes. Deferred, for three reasons: it only works
for HTTP/1.1 (HTTP/2 headers are HPACK-compressed frames, not text), it needs careful
keep-alive handling to know where one request's headers end, and it is gold-plating before
the basic capture path exists. It stays cheap to add later — a `raw_headers bytea` column is
one migration, and the schema is designed with room for it.

**Restore `Host` and `Transfer-Encoding` by hand.**
`net/http` moves `Host` to `r.Host` and `Transfer-Encoding` to `r.TransferEncoding`, deleting
both from the map. A naive `range r.Header` therefore silently drops **the single header that
determined which inbox the request landed in**. Pinned by `TestFromHTTPRestoresHost`, which
also asserts the precondition — if a future Go release stops removing `Host`, that test fails
loudly rather than letting us double-add it.

**`netip.Addr`, not `net.IP`.**
`netip.Addr` is comparable, usable as a map key, and does not allocate. `net.IP` is a
`[]byte` with none of those properties. Phase 5's per-IP rate limiting wants a map key.

**`r.RemoteAddr`, deliberately not `X-Forwarded-For`.**
XFF is a header, which means the client writes it, which means an unauthenticated caller can
put anything there. Trusting it without being certain you sit behind a proxy that *overwrites*
it is how IP-based rate limits get bypassed. When hooklens is deployed behind a load
balancer this needs an explicit list of trusted proxy addresses — Phase 5, and better left
obviously wrong now than subtly wrong forever.

**Parsing lives in its own package, separate from the handler.**
`capture.FromHTTP` takes an `*http.Request` and returns a value. No database, no logger, no
`ResponseWriter`. That is what let this unit ship 13 tests with no server, no container and
no fixtures — the body/header/truncation logic is where the bugs live, and it is now testable
in microseconds.

## Walkthrough

### `internal/capture/request.go`

**`Request` (`:41`)** is a plain value with no methods that do I/O. `Body []byte` is the
load-bearing field and the comment above it is a warning to the future: parse it and Phase 4
breaks.

`DeclaredSize` (`:60`) is named for what it is. Calling it `Size` would invite someone to
trust it; it is the sender's `Content-Length` and the sender may be wrong or absent. The live
test showed all three states: `42` matching reality, `5242880` on a body we truncated to
`1048576`, and `-1` under chunked encoding.

**`readBody` (`:106`)** is the unit's core, and every line of it is deliberate.

`io.LimitReader(rc, maxBody+1)` — the `+1` is the entire truncation-detection mechanism.
Remove it and `Truncated` can never be true, silently, with no test failing unless one
asserts the boundary. `TestFromHTTPBody` has cases at `limit-1`, exactly `limit`, and
`limit+1` for that reason.

The error branch (`:119`) returns `body` *and* the error, rather than `nil, err`. That looks
sloppy and is not: a partial body is the evidence. `TestFromHTTPPartialBody` asserts both
halves — an error is returned, and the five bytes that arrived are kept.

The non-draining return (`:127`) is the security decision made concrete. What breaks if you
"fix" it by adding `io.Copy(io.Discard, rc)`: an attacker sends an infinite body, you read
all of it, memory is bounded but *time* and the goroutine are not.

**`headersOf` (`:151`)** — the doc comment is longer than the function on purpose, because
the function looks trivial and the constraints behind it are not.

`slices.Sorted(maps.Keys(r.Header))` gives deterministic ordering. The inner loop over
`r.Header[name]` preserves arrival order *within* a name, which is the multi-value property
that `map[string]string` would destroy.

The `Host` and `Transfer-Encoding` restorations (`:162`, `:168`) come after, then the whole
slice is re-sorted with `SortStableFunc` so the restored entries land in name order instead
of always trailing. **Stable** matters: it is what keeps two `X-Custom` values in arrival
order through the second sort. Swap it for `SortFunc` and that ordering becomes undefined,
and `TestFromHTTPHeaders` would fail intermittently rather than reliably — the worst kind.

**`sourceIP` (`:196`)** ignores the `SplitHostPort` error and falls back to the raw string,
for the same reason as `normalizeHost` in `routing.go`: the error means "no port", which is
a legitimate shape. `addr.Unmap()` converts IPv4-mapped IPv6 (`::ffff:203.0.113.9`) to plain
IPv4, so the same client is one key rather than two.

### `internal/ingest/ingest.go`

`capture` (`:79`) now reads for real. The ordering at the end is the point: the log line and
every fallible operation happen *above* `writeJSON`, and the comment marks where the database
insert goes in unit 07. Once we write 200 the provider considers the event delivered and will
not resend it, so nothing that can fail may sit between the durable record and that line.
This is Phase 0 quiz Q1 turned into a structural constraint rather than a note.

The `req == nil` guard (`:88`) is unreachable today, since `FromHTTP` only returns a nil
`*Request` on an error path that currently cannot trigger. It stays because the alternative
is a nil dereference in the most hostile-input-facing function in the project.

## Verified

13 tests, all passing, plus live exercise of the built binary:

| Sent | `body_bytes` | `declared_size` | `truncated` |
|---|---|---|---|
| 42-byte JSON | 42 | 42 | false |
| 7 bytes of gzip magic + NULs | 7 | 7 | false |
| 5 MB of `x` | **1048576** | **5242880** | **true** |
| chunked, no Content-Length | 13 | **-1** | false |
| `PATCH ?a=1&b=two` | 1 | 1 | false |

Row three is the whole unit in one line: we kept exactly the cap, we recorded the sender's
claim of 5 MB separately from what we actually hold, and we know we truncated. Row four is
why the `+1` probe exists rather than trusting `Content-Length`.

Raw-byte fidelity is pinned by two tests that will fail the moment anyone parses on the way
in: `TestFromHTTPBodyIsRaw` (duplicate JSON keys, odd whitespace, trailing newline all
survive byte-for-byte) and `TestFromHTTPBodyIsNotUTF8` (NUL and invalid UTF-8 survive).
