# 08 — Capability URLs: entropy, and why tokens are hashed

*Phase 1, unit 3. Covers the curriculum bullet "capability URLs as authentication: entropy,
why unguessable is enough, why tokens are hashed at rest".*

## Brief

**What is it?** A capability URL grants access by virtue of being known. There is no login,
no account, no session. Possessing the URL *is* the authorisation.

You use these constantly: a Google Docs "anyone with the link" share, a password-reset
email, a Dropbox link, an unsubscribe link. The name comes from capability-based security —
instead of checking an access-control list against an identity, you hold an unforgeable
token that *is* the permission.

**What problem does it solve?** Normal authentication is two questions: who are you
(login), and what may you do (authorisation). Answering the first requires accounts,
password storage, session management, reset flows, email verification. That is weeks of
work and a permanent liability — you now hold credentials someone wants to steal.

For a great many cases the first question is irrelevant. Nobody needs to know *who* is
looking at a captured webhook; they need to know the looker was given access. A capability
URL answers only the second question, and therefore needs none of that machinery.

**How does it work underneath?** Entirely on **entropy** — how unguessable the identifier
is.

*n* bits of entropy means 2^n possible values. The standard bar is 128 bits, the same as an
AES key. That is 3.4 x 10^38 values: at a million guesses per second — far beyond what any
rate-limited endpoint permits — you would expect a hit in about 10^25 years.

Where the bits come from matters as much as how many. They must come from a
cryptographically secure source — `crypto/rand`, which draws from the operating system's
entropy pool — and never from `math/rand`, which is a deterministic function of a seed and
becomes fully predictable once an attacker observes a handful of outputs.

**Why hash the token at rest?** Because your database is not a secret. Backups get copied
to laptops, dumps get shared to debug something, query parameters land in logs, and one SQL
injection exposes every row. Store tokens verbatim and anyone with read access to that
table holds every capability instantly. Store only a hash and they hold nothing usable.

Now the part people get wrong. For a *user-chosen password* you need a deliberately slow
hash — bcrypt, argon2, scrypt — because human passwords have perhaps 20-30 bits of entropy
and are brute-forceable, so you make each guess expensive. For a *randomly generated
256-bit token* there is nothing to brute force; the search space is already astronomical. A
single SHA-256 is the correct choice. Using bcrypt here buys no security and adds latency
to every request.

**Sharp edges.**

- **URLs leak, and they leak somewhere very visible.** Browser history, `Referer` headers
  when a page links outward, access logs, proxy logs, chat link previews, screen shares. A
  capability URL is a bearer credential written in the most quotable place in the request.
  This is the real objection to the technique, and the mitigations are narrow scope, expiry,
  and `Referrer-Policy: no-referrer`.
- **Unique is not the same as unguessable.** A sequential id is perfectly unique and
  trivially guessable. So is anything derived from a timestamp.
- **UUIDv7 must never be used as a secret.** This matters directly to us: v7 is 48 bits of
  *knowable* timestamp plus 74 random bits. Fine as a row id, useless as a capability.
  UUIDv4 has 122 random bits and is acceptable.
- **No identity means no audit trail**, and revocation needs machinery you must build
  deliberately.

**In hooklens.** Two different secrets, because they have genuinely different exposure. The
**slug** goes in the URL you paste into Stripe's dashboard — it lives in their config, your
logs and your terminal history, so it is semi-public by design. It gets 128 bits so nobody
can *find* your inbox by scanning, but learning it only lets someone send you junk. The
**token** gates reading and managing, is shown exactly once at creation, and is stored only
as a SHA-256 hash. Learn the slug and you can fill someone's inbox; you still cannot read a
byte of it.

## Decisions

**Two secrets, not one, because they have different exposure.**
The **slug** is pasted into Stripe's dashboard, so it lives in their config, our access
logs, and your terminal history. It gets 128 bits so nobody can *find* an inbox by scanning,
but it is treated as semi-public: learning it lets someone send junk, nothing more. The
**token** gates reading and management, is shown exactly once, and exists in the database
only as a SHA-256.
*The rejected alternative* is webhook.site's model, where one URL does both. Simpler, and
it means anyone who sees the URL in a provider's config can read every payload that inbox
ever received — including the ones with live API keys in them. The split costs one header.

**The capture path deliberately requires no token.**
A provider posting a webhook has no credential and never will. `EndpointBySlug` therefore
authenticates nothing, and that is correct rather than a gap: the slug's entropy is what
protects the inbox from discovery, and the token is what protects its *contents*.
Verified: `POST /e/{slug}/webhook` with no header returns 200, and reading that same inbox
without a token returns 401.

**Every authentication failure returns the identical response.**
Missing inbox, wrong token, malformed header — all produce `401 {"error":"missing or
invalid token"}`, byte for byte. Distinguishing "no such inbox" from "wrong token" would
turn a 2^128 guessing problem into a lookup: an attacker could confirm which slugs exist,
then attack only those. This is why `store.ErrUnauthorized` is returned for `pgx.ErrNoRows`
as well as for a hash mismatch (`internal/store/endpoints.go:124`).
Verified live: all three cases produce identical bytes.

**SHA-256, not bcrypt/argon2/scrypt.**
Slow hashes exist to make brute force expensive, which matters when the secret is a
human-chosen password holding 20-30 bits of entropy. This token holds 256 bits from a
CSPRNG — there is nothing to brute force, and the search space is already beyond reach. A
slow hash would add latency to every authenticated request and buy nothing.
*Wrong call if* the token ever became user-chosen. *Secondary reason:* bcrypt silently
truncates input at 72 bytes, which is how a "more secure" choice becomes a weaker one.

**base32 for the slug, base64url for the token.**
base32 because the slug goes in a hostname: DNS is case-insensitive, so a base64 slug
breaks the moment a resolver or a provider's config normalises its case, and `+`/`/` are
not legal in a label anyway. Lowercased base32 emits only `[a-z2-7]`, a subset of the
`[a-z0-9-]` whitelist in `server.validSlug`, so a generated slug is always a routable one —
pinned by `TestNewSlugShape`. The cost is density: 5 bits per character against base64's 6,
so 128 bits needs 26 characters instead of 22.
The token only travels in a header, where density wins and case does not matter.

**Look up by slug, compare the hash in Go.**
`where owner_token_hash = $1` would have been one query instead of a query plus a
comparison. Rejected twice over: an index lookup's timing is emphatically not constant, and
the hash would land in the Postgres query log. Looking up by the non-secret and comparing
the secret in constant time keeps the secret out of the database's own logs.

**Token in the `Authorization` header, never a query parameter.**
A token in a query string lands in the server access log, the proxy log, the browser
history, and the `Referer` header of any outbound link — which is exactly the leak that
makes people distrust capability URLs. The header costs nothing; the UI is JavaScript and
can set it.

**Retry on slug collision rather than returning an error.**
At 128 bits the loop will never run twice. It exists because "never" and "returns a
confusing error to a user who did nothing wrong" are different guarantees, and being right
costs four lines (`internal/store/endpoints.go:53`).

**Existing endpoints were made permanently unreadable by the migration.**
They were created before tokens existed, so there is no correct value — they were never
issued one. `00003` backfills them with the hash of a random value, which no token can
match. The inbox keeps receiving; it can never be read again. Inventing a token nobody was
given would be worse, and leaving those rows readable by anyone worse still.

**`Equal` rejects empty input explicitly.**
`subtle.ConstantTimeCompare(nil, nil)` returns 1 — two empty slices genuinely are equal.
Without the guard, an empty stored hash would authenticate an empty token. `Hash` always
returns 32 bytes so no legitimate caller can reach that state, which is precisely why it
must be handled: the only route there is something already broken. Found by
`TestEqual`, which failed on the first run.

## Walkthrough

### `internal/secret/secret.go`

`NewSlug` (`:43`) and `NewToken` (`:65`) both ignore the error from `crypto/rand.Read`.
Since Go 1.24 that function cannot fail — it panics instead of returning an error — which
was a deliberate change by the Go team on the grounds that a silently ignored RNG failure
is worse than a crash. There is genuinely no error path to handle.

`slugEncoding` (`:38`) is the one line with three separate reasons behind it, all in the
doc comment: case-insensitivity for DNS, no `+`/`/` for hostnames, and an output alphabet
inside `validSlug`'s whitelist.

`Equal` (`:91`) — see the decision above. The length guard is not defensive noise; it is
the difference between "cannot authenticate" and "authenticates on empty".

### `migrations/00003_endpoint_tokens.sql`

The three-step add-a-NOT-NULL-column pattern, and each step has a different cost profile.
Step 1 (add nullable) is metadata-only and instant on Postgres 11+. Step 2 (backfill) is a
full table write. Step 3 (`set not null`) takes an `ACCESS EXCLUSIVE` lock and scans every
row to prove no nulls remain — brief on a table with single digits of rows, minutes on a
large one, during which all reads and writes block. The production-safe variant is a
`CHECK (... IS NOT NULL) NOT VALID` plus a separate `VALIDATE` under a weaker lock; noted
in the file rather than done, because the complexity is not warranted here.

No index on `owner_token_hash`, deliberately. Lookups go by slug, and an index on a secret
makes it enumerable through timing.

### `internal/store/endpoints.go`

`AuthenticateEndpoint` (`:109`) selects the hash and compares in Go. The `pgx.ErrNoRows`
branch returns `ErrUnauthorized`, not `ErrNotFound` — that single substitution is what
makes a missing inbox indistinguishable from a bad token.

`AuthenticateRequest` (`:144`) joins `requests` to `endpoints` in one query rather than
fetching the request and then checking ownership. There is therefore no window in which the
row has been read out of the database on behalf of a caller who turns out not to be allowed
it. Verified: another inbox's valid token gets 401 on someone else's request.

### `internal/server/server.go`

`bearerToken` (`:110`) uses `strings.EqualFold` on the prefix because RFC 9110 makes the
scheme name case-insensitive — `bearer x` is as valid as `Bearer x`, and rejecting it would
be a bug that only shows up with one particular HTTP client.

`unauthorized` (`:125`) is a single function rather than inline responses precisely so the
three call sites cannot drift apart. The moment one of them says something slightly
different, the oracle exists.

`handleCreateEndpoint` (`:132`) tolerates `io.EOF` from the decoder (`:139`), so `POST`
with no body at all creates an unnamed inbox instead of a 400.

## Verified

13 new tests (5 in `secret`, 4 new in `store`), all passing; 8 store tests total.

`TestTokenNotStoredInPlaintext` queries the database directly, the way a leaked dump would,
and asserts the stored bytes are a 32-byte SHA-256 that does not contain the token.

`TestAuthenticateEndpoint` covers wrong token, empty token, garbage token, another inbox's
slug, and a nonexistent slug — all must return the same `ErrUnauthorized`.

End to end against the running binary:

```
slug  : wgwzuqveikhjr6rbkcupx747c4   (26 chars, 128 bits)
token : gvZHsKVuMJ-Y...              (43 chars, 256 bits)

POST /e/{slug}/webhook  no token      -> 200        (providers have no credential)
GET  .../requests       no token      -> 401 {"error":"missing or invalid token"}
GET  .../requests       wrong token   -> 401 {"error":"missing or invalid token"}
GET  .../requests       bad slug      -> 401 {"error":"missing or invalid token"}
GET  .../requests       right token   -> 200 + the captures
GET  /api/requests/{id} other's token -> 401
GET  /api/requests/{id} owner's token -> 200
```

The three 401 bodies are byte-identical, which is the point.

And the database holds only the hash:

```
stored : e10e0e1b3fe30e85df56b90420a25f4c21435799fb677654b965b42038521695
issued : gvZHsKVuMJ-YVBiG8lrglV8-VjcNoyaRofVhE3Smu0A
```
