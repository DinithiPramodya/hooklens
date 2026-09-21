# 26 — Hashes, MACs, HMAC, and comparing without leaking

*Phase 4, unit 1. Covers the curriculum bullets "hash functions", "MAC vs. signature, and
why a bare hash is not enough", "how HMAC is constructed and why it is built that way",
and "timing attacks and constant-time comparison".*

## Brief

**What is this thing?** An HMAC is a short tag computed from a message and a secret key,
which anyone holding the key can recompute to check that the message is authentic and
unmodified. Stripe, GitHub, Shopify and Slack all put one in a header on every webhook.

**What problem does it exist to solve?** A webhook endpoint is a URL on the public
internet that accepts POSTs. Anyone who learns it can send you a payment-succeeded event
you never earned. The URL being unguessable helps, but it leaks — into logs, proxies,
screenshots — and it is a *bearer* secret, so knowing it is enough to forge with it.

What is needed is proof the message came from someone holding a shared secret. Three
attempts, in order of how people usually reach for them:

**A plain hash of the body.** `sha256(body)` proves the body was not corrupted in transit.
It proves nothing about *who* sent it, because the hash function is public — an attacker
computes the same hash over their own body. A hash is an integrity check, not an
authentication check.

**A hash of the secret and the body concatenated**, `sha256(secret || body)`. Now only
someone with the secret can produce it, which sounds right and is broken. Hash functions
in the SHA-2 family are **Merkle–Damgård** constructions: they absorb a message block by
block into an internal state, and the digest *is* that final state. Given a valid
`sha256(secret || body)`, an attacker can load it back as the starting state and keep
absorbing — producing a valid tag for `secret || body || padding || anything-they-like`
**without ever knowing the secret**. That is a **length-extension attack**, and it is why
the obvious construction is not merely inelegant but wrong.

**HMAC**, which is the fix: `H(key ⊕ opad || H(key ⊕ ipad || message))`. Two passes with
two different key-derived pads. The inner hash produces a fixed-size digest, and the outer
hash absorbs *that* rather than the message — so an attacker extending the inner result
cannot get past the outer one without the key. The nested structure is the whole point of
the design, not decoration.

**MAC, not signature.** Both words get used loosely; they are different. A MAC uses **one
shared secret**, so verification requires the same key used to sign — which means the
verifier could also have forged the message, and a MAC proves nothing to a third party. A
signature uses a **key pair**: anyone with the public key can verify, nobody but the holder
of the private key can produce. Webhooks use MACs because both ends already share a secret
and MACs are far cheaper.

**How do you compare the tags?** Not with `==`. String comparison returns on the first
differing byte, so how long it takes leaks *how much of the prefix was right* — and an
attacker who can measure that recovers the tag one byte at a time, turning an infeasible
search into a few thousand requests. The comparison must take the same time regardless of
where the difference is.

**What are the sharp edges?**

- **Byte-exactness.** The MAC is over exact bytes. Parsing JSON and re-serialising it
  changes key order and whitespace and produces a different tag, so verification must run
  over the body *as received* — which is why it is stored as `bytea`.
- **Length leaks even from constant-time compare**, so tags of different lengths must be
  rejected before comparing rather than inside the loop.
- **HMAC is not encryption.** The payload is still plaintext to anyone who sees it.

**In hooklens:** an `internal/signature` package computes HMACs, compares with
`hmac.Equal`, and shows the developer the exact bytes that were signed — because "invalid"
with no explanation is the least useful thing this tool could say.

## Decisions

**The `Result` struct carries the canonical string, always — valid or not.**
The decision the package is built around. Nearly every real signature bug is a
canonical-string bug: a missing newline, a re-serialised body, the wrong separator, hex
where base64 was wanted. None of them is diagnosable from a boolean, and a tool that
answers "INVALID" and stops is barely better than the provider's own dashboard. `Signed`,
`Expected` and `Provided` are populated on every path that gets far enough to compute
them.

**`Problem` and `Hint` are separate fields.**
"What is wrong" and "what do I do" are different questions, and a developer staring at a
red panel needs both. Merging them produces a sentence that answers neither well.

**The hints diagnose rather than list possibilities.**
`stripeHint` checks for surrounding whitespace before blaming the secret, because a
framework that trimmed or re-serialised the body is the most common real cause and the
secret is usually fine. `VerifyShopify` goes further and *recomputes the tag in hex* — if
that matches, it says so outright: "the signature is correct but HEX-encoded". That is
the difference between a tool and a checkbox.
*Wrong call if* a guess is confidently wrong, which is why each is phrased as a likely
cause and the raw fields are shown alongside it.

**`now` is a parameter, not `time.Now()`.**
Two reasons, and the second is the one that matters. It makes the replay window testable
without sleeping — and it lets a stored capture be re-verified against **the time it
arrived**. Using the wall clock would make every capture read "expired" five minutes
after it landed, which is true and useless.

**The replay window is checked *after* the signature, not before.**
Checking the cheap thing first is the reflex. It is wrong here because it reports two
different situations identically: an expired request with a *valid* tag is a slow
delivery or a clock skew; an expired request with a garbage tag is an attack. The
messages differ, so the order has to.

**A future timestamp gets its own message.**
Two minutes in the future is never a replay; it is a clock problem, and the hint says NTP
rather than "this is normal for a retry".

**Any matching `v1` passes, not just the first.**
Stripe sends several during a secret rotation. A verifier that checks only the first
works perfectly until the day someone rotates a secret — the worst possible time to
discover it. There is a test that puts the *old* secret's tag first.

**`v0` is ignored rather than accepted.**
Stripe's legacy test-mode scheme. Accepting it would mean accepting a weaker check this
package has not implemented.

**Comparison is over the encoded form, not the decoded bytes.**
Slightly worse cryptographic hygiene, chosen deliberately: providers differ in hex case,
and a developer whose tag differs only in case needs to *see* that rather than be told
"invalid". Case is normalised at the call site where the provider's spec allows it, and
`Provided` still shows what actually arrived.

**Length is checked before the constant-time compare.**
Tag length is not secret — it is fixed by the algorithm and visible in the header — and
no constant-time primitive defines behaviour for inputs of different lengths.

**`hmac.Equal`, not `subtle.ConstantTimeCompare`.**
The same function with a clearer name at the call site, and it avoids the surprise that
cost a test in unit 08: `ConstantTimeCompare(nil, nil)` returns 1.

**SHA-1 is supported and flagged.**
Shopify and older GitHub webhooks use it. The comment states the distinction that makes
this defensible rather than lazy: SHA-1 is broken for *collision resistance*, which
matters for signatures over attacker-chosen documents. HMAC's security rests on the hash
being a decent PRF, and **HMAC-SHA1 remains unbroken**. Knowing which property a
construction actually needs is the difference between cargo-culting "SHA-1 is broken" and
being right.

**The secret is a parameter and is never stored.**
It belongs to the user's integration and has no use beyond the one call. Storing other
people's webhook secrets would make this service worth attacking for a reason unrelated
to what it does. The endpoint is `POST` for the same reason: a `GET` would put the secret
in a query string, which lands in access logs, proxy logs, browser history and the
`Referer` of any outbound link — the exact leak that makes people distrust capability
URLs ([08](08-capability-urls.md)).

**`internal/signature` declares its own `Header` type.**
The third package to do so, after `store` and `tunnel`, and for the same reason: it is a
leaf, and a verifier that drags in the storage layer cannot be reused or tested cheaply.

## Walkthrough

### `internal/signature/signature.go`

`Compute` (`:58`) — the primitive. The ignored `Write` error is deliberate and commented:
hashes document that they never fail, and a checked error there would imply a failure
mode that does not exist.

`Equal` (`:80`) — the length guard, then `hmac.Equal`, with the reasoning for comparing
encoded rather than decoded.

`Result` (`:97`) is shaped for display, not for control flow. That is unusual for a
"verify" function and is the point of the package.

### `internal/signature/stripe.go`

`VerifyStripe` (`:35`) in order: parse, canonical string, compute, compare every `v1`,
*then* the window.

`parseStripeHeader` (`:106`) ignores `v0`.

### `internal/signature/providers.go`

The three others, each with its scheme in the doc comment. Worth reading together: four
teams solved one problem four different ways, and the differences are exactly where
hand-written verifiers break.

`VerifyGitHub` (`:35`) — note the comment on **no replay protection at all**. GitHub signs
the bare body, so a captured delivery can be resent forever and will verify; their answer
is `X-GitHub-Delivery` and application-level deduplication, which most applications do
not do.

`VerifyShopify` (`:84`) — the hex/base64 diagnosis.

`VerifySlack` (`:121`) — the version lives *inside* the signed string as well as on the
tag, so a future `v1` cannot be confused with a `v0`. A small piece of good design that
Stripe's `t.body` does not have.

### `internal/server/verify.go`

`POST /api/requests/{id}/verify`, and the comment on why it is a POST.

## Verified

**29 tests across four providers**, several of which exist to pin properties rather than
behaviour:

- `TestComputeMatchesTheStandardLibrary` anchors against `crypto/hmac` directly, so a
  divergence shows up here rather than as four confusing provider failures.
- `TestComputeIsSensitiveToEveryByte` feeds five variants differing only in whitespace and
  asserts all five produce different tags — the property that makes "sign the raw bytes"
  non-negotiable.
- `TestEqual` covers the last-byte difference, the length mismatch, and the empty/empty
  case that surprised us in unit 08.
- `TestVerifyStripeRotation` puts the **old** secret's tag first, so a first-match-only
  verifier fails it.
- `TestVerifyStripeDetectsMangledBody` asserts the hint points at the whitespace rather
  than at the secret.
- `TestVerifyShopifyDiagnosesHexEncoding` asserts the problem names the *encoding*.
- `TestEveryProviderShowsItsCanonicalString` walks all four and requires `Signed`,
  `Expected` and `Provided` on each.
- `TestProvidersDisagree` asserts the four canonical strings are genuinely different — a
  guard against a refactor quietly collapsing two providers onto one code path.

**Six endpoint tests**, including `PLAN.md`'s criterion directly: corrupting the secret
reads invalid **and shows the string it compared**. Plus one asserting the response never
echoes the secret back, because a leak into a browser cache or a screenshot would be
entirely our fault.
