# 27 — Replay: the same mechanism as the attack

*Phase 4, unit 2. Replaying a stored capture to the tunnel or to an arbitrary URL, and
editing it first. Builds on [26](26-hmac.md), where replay was the thing timestamps exist
to prevent.*

## Brief

**What is this thing?** Taking a capture we already hold and sending it again — to the
developer's local app through the tunnel, or to a URL they name — optionally with the body
edited first.

**What problem does it exist to solve?** It removes the slowest loop in webhook
development. Without it, testing a change to a handler means going back to the provider's
dashboard, triggering a real event, waiting for it to arrive, and hoping it is the same
shape as last time. Some events are hard to trigger at all: a dispute, a subscription
ending, a failed payment on a specific card. With replay, you catch the event **once** and
then iterate against it as many times as you like, in a second each.

Edit-and-replay extends that to events you cannot trigger at all. Change `amount` to a
number that overflows your column; change `status` to a value the provider has not shipped
yet; delete a field to see whether your parser copes. These are the tests worth writing
and the events you will never receive on demand.

**How does it actually work underneath?** It is a stored request replayed through the
existing forwarding path, which is nearly all of the implementation. The interesting part
is that **this is the attack from unit 26**. A replay attack is an adversary resending a
captured request; a replay feature is a developer resending a captured request. The bytes
are identical. The difference is entirely *who is asking and where it goes* — which means
the design work is authorisation and destination control, not mechanism.

Two consequences follow, and the second is the one that bites.

**A replayed request will usually fail signature verification** if the scheme includes a
timestamp — correctly, because that is exactly what the timestamp is for. An edited body
fails always, because the MAC covers the bytes. The tool must say this rather than let a
developer conclude their verification code broke.

**Replaying to an arbitrary URL turns the server into a confused deputy.** If hooklens will
POST attacker-chosen bytes to an attacker-chosen URL, then anyone with an inbox can use our
server to reach things *it* can reach and they cannot: `localhost`, private ranges, and on
a cloud host the link-local metadata endpoint at `169.254.169.254`, which hands out
credentials to anything that asks. That is **SSRF**, and "the user asked us to" is not a
defence, because the user is the attacker.

**What are the sharp edges?**

- **Blocking by hostname does not work.** `evil.com` can resolve to `127.0.0.1`. The check
  must be on the resolved IP.
- **Nor does checking once.** Resolve, check, then connect and the name can resolve
  differently the second time — a **DNS rebinding** race. The check has to happen on the
  address actually dialled.
- **Redirects escape the check entirely** unless they are refused or re-checked: a public
  URL can 302 to `169.254.169.254`.
- **A replay is indistinguishable from the real thing downstream** unless it is marked, so
  it needs a header saying so — otherwise a developer's own logs lie to them.

**In hooklens:** replay goes to the tunnel by default. An arbitrary URL is allowed, guarded
at the dial with a private-range block, redirects refused, and every replayed request
carries `X-Hooklens-Replay: 1`.

## Decisions

**Arbitrary URLs are allowed, not banned.**
The safe option was tunnel-only, and it would have removed a genuinely useful case:
replaying against a staging deployment, or a colleague's tunnel, or a webhook.site-style
scratch endpoint. The feature is worth the defence, and the defence is not large. What
would change this: if the server were ever multi-tenant with untrusted users at scale, the
risk calculus shifts and an allowlist beats a blocklist.

**The guard lives in `Dialer.Control`, not before the request.**
The decision that matters. Resolving the hostname ourselves, checking the result, and then
letting `http.Client` resolve it *again* leaves a window where the second answer differs —
**DNS rebinding**, and a check that runs earlier than the connect is a check an attacker
can race. `Control` runs with the literal address the kernel is about to use. There is a
test that reaches a loopback server by the name `localhost` and asserts it never arrives.

**`CheckURL` exists anyway, and is explicitly not the security control.**
It runs first for the *error message*: a bad scheme or a literal private IP should be
reported as such immediately rather than as a dial failure after a DNS lookup. The doc
comment says outright which of the two is load-bearing, because a future reader deleting
the "redundant" one needs to delete the right one.

**`Unmap()` before every check.**
`::ffff:127.0.0.1` is loopback, and every `Is*` predicate returns **false** for it in its
mapped form. That is the classic bypass for this exact function, and it is one line.

**An explicit list of extra ranges, because the predicates are not enough.**
Found by a failing test. The first version asserted in a comment that
`!IsGlobalUnicast()` would catch carrier-grade NAT; it does not. `IsGlobalUnicast` is
about the **addressing architecture**, not about whether a packet routes on the public
internet — so 100.64/10 is "global unicast" and is also somebody's ISP-internal network.
The comment was wrong and the test caught it; both the fix and the correction are in the
code.

**Redirects are refused, not re-checked.**
Following them and applying the dial guard per hop would also work. Refusing is one line,
and it avoids a security-relevant path that nothing exercises — a public URL 302'ing to
`169.254.169.254` is the textbook bypass. For a replay, seeing the 302 is information
rather than an obstacle.

**`Proxy: nil` on the transport.**
`http.ProxyFromEnvironment` is the default, and an `HTTP_PROXY` in the server's
environment would route every replay through it — around the dial guard entirely. The
destination belongs to the user; the server's proxy configuration has no business in it.

**Every replay carries `X-Hooklens-Replay: 1`.**
Not optional. A replay is byte-identical downstream, so without a marker a developer's own
logs cannot distinguish "the provider sent this twice" from "I resent it" — and telling
those apart is the confusion this whole tool exists to remove.

**An edited replay says the signature will no longer match.**
A MAC covers the bytes; change them and it cannot verify. Obvious in hindsight and not
obvious at 2am, and the alternative is a developer concluding their verification code
broke.

**Editing headers *replaces* rather than merges.**
A partial merge has no obvious semantics for a repeated header — is `Set-Cookie` replaced
or appended? — and "send exactly these" is what an editor produces anyway.

**A fourth copy of the hop-by-hop list.**
`internal/ingest`, `internal/tunnel` and now `internal/replay` each have one. Three copies
of nine strings is less coupling than a utility package every layer depends on, and this
one legitimately differs: it also drops `Host`, which the others keep.

## Walkthrough

### `internal/replay/safedial.go`

`blocked` (`:35`) — the `Unmap`, the ordered cases with their specific messages, and then
`extraBlocked` for what the predicates miss.

`safeTransport` (`:79`) — the `Control` hook, with the rebinding argument above it, and
`Proxy: nil`.

`SafeClient` (`:124`) — `CheckRedirect` and why refusing beats re-checking.

`CheckURL` (`:137`) — labelled as the convenience, not the control.

### `internal/replay/replay.go`

`ToURL` (`:69`) appends the capture's path to the target's, matching the tunnel client so
`--to localhost:3000/api` and a replay to `http://host/api` behave the same way.

`parseHTTPURL` (`:126`) restricts the scheme. Not pedantry: a permissive parse lets a
target through to code that assumes it was checked.

### `internal/server/replay.go`

`handleReplay` (`:26`) applies the edits to the stored capture, then forks on destination.
The `edited` flag drives the signature note.

`replayToURL` (`:152`) distinguishes an `ErrBlockedDestination` (a 400 with an
explanation) from any other failure (a 200 with the outcome recorded), because the first
is the user asking for something we refuse and the second is the world being unreliable.

## Verified

**Twenty-two tests**, and the SSRF list is enumerated rather than gestured at.
`TestBlockedAddresses` asserts seventeen blocked addresses — loopback in three forms
including IPv4-mapped, the cloud metadata address, both IPv6 link-local and unique-local,
all three RFC 1918 ranges, carrier-grade NAT, multicast, unspecified — and six that must
still be **allowed**, including `172.15.0.1` and `172.32.0.1`, the addresses either side of
the private 172.16/12 block that hand-written checks most often get wrong.

`TestSafeClientBlocksAtTheDial` is the load-bearing one: it stands up a real loopback
server, reaches it by the name `localhost` so the literal-IP pre-check cannot be what
stops it, and asserts the handler is never entered and the error is an
`ErrBlockedDestination` naming loopback.

At the HTTP layer, `TestReplayToURLRejectsRebinding` repeats that end to end through the
API, and `TestReplayToTunnel` asserts the round trip: the app sees the request a **second**
time, carrying `X-Hooklens-Replay`, with the original body — and the header was absent on
the first delivery, so the test cannot pass by accident.
