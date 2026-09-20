# 12 — What CORS actually is, and how single-origin sidesteps it

*Phase 2, unit 2. Covers the curriculum bullet "**what CORS actually is**, why it exists,
and how single-origin sidesteps it entirely".*

## Brief

**What is it?** A set of HTTP headers by which a server tells a **browser** that a page from
one origin is allowed to *read* a response from another.

The framing that makes everything else make sense: **CORS is a browser rule, not a server
rule.** It is enforced by the browser, against JavaScript running in a page, on the user's
behalf. `curl` has no CORS. Your Go server has no CORS. A CORS error never means the server
rejected you — it means the browser fetched the response and then refused to hand it to the
page.

An **origin** is the triple *(scheme, host, port)*. `http://localhost:5173` and
`http://localhost:8080` are different origins because the ports differ. `http://x.com` and
`https://x.com` differ by scheme. Same machine is nowhere near enough.

**What problem does it solve?** The same-origin policy came first, and CORS only makes sense
as a relaxation of it. The policy says: a page may not read data from another origin. The
reason is that **browsers attach credentials automatically**. You are logged into your bank.
You visit `evil.com`. Its JavaScript runs `fetch('https://bank.com/accounts')` — and the
browser dutifully attaches your bank cookies, because that is what cookies do. Without the
same-origin policy, `evil.com` reads your balance. The policy is the thing that makes
ambient credentials survivable at all.

But that blanket rule is too strict for a legitimate API on `api.example.com` serving a page
on `app.example.com`. CORS is the opt-in: the server names which origins may read it.

**How does it work underneath?** The browser adds an `Origin:` header to the cross-origin
request. The server answers with `Access-Control-Allow-Origin`. The browser compares the
two. On a mismatch it **discards the response** and hands JavaScript an opaque error — but
note the request was still sent and the server still executed it. That is why a "blocked by
CORS" request can still have changed your database.

Then there is **preflight**. For anything beyond a *simple request*, the browser sends an
`OPTIONS` probe first, carrying `Access-Control-Request-Method` and
`Access-Control-Request-Headers`, and only sends the real request if the server approves.
"Simple" means GET/HEAD/POST with a content type from a short list — form-encoded, plain
text, multipart — and no custom headers.

Which means `Content-Type: application/json` triggers a preflight. So does `Authorization:`.
In practice **almost every real API call preflights**, turning one round trip into two, on
every request, unless you set `Access-Control-Max-Age` to let the browser cache the
approval.

**Sharp edges.**

- **`Access-Control-Allow-Origin: *` and credentials are mutually exclusive.** Send
  `credentials: 'include'` and the browser demands an exact origin echo; `*` is rejected. So
  you end up echoing the request's `Origin` back — and if you echo it without validating
  against an allowlist, you have just allowed *every* origin, including the attacker's,
  while appearing to have configured something.
- **A CORS failure is not a server failure.** Devtools shows red; the server log shows
  `200`. People spend hours debugging the wrong side.
- **Preflights are invisible until they are not.** Adding one header can silently double
  your request count.
- **CORS is not access control for your server.** It protects a *user's browser session*.
  Anyone can `curl` your API regardless. Treating it as authorization is a genuine security
  mistake.

**In hooklens.** None of this applies, and that is the point. One binary serves the page and
the API, so every request is same-origin and there is not a single CORS header anywhere in
the codebase. Vite's dev proxy preserves that in development — the browser only ever talks
to `:5173`. The alternative was CORS configuration in three places, plus a preflight before
every authenticated call, because our `Authorization` header guarantees one.

## Decisions

**No CORS headers anywhere, and a test that fails if anyone adds one.**
This is the only decision in the unit, and the code change is an *absence* — which is
exactly why it needs pinning. Absences do not survive contact with a future developer
staring at a red devtools error, googling it, and pasting in a CORS middleware. By the time
that happens the origins will already have been split, and the middleware will paper over
the real change rather than reveal it.
`TestNoCORSHeaders` asserts no response on any route carries an `Access-Control-*` header.
It is not testing the HTTP spec; it is testing *our architecture*, and it converts a silent
drift into a failing build that has to be argued with.
*Wrong call if* hooklens ever legitimately serves an API to a page it does not host — a
public embeddable widget, say. Then the test should be changed deliberately, alongside an
origin allowlist, which is precisely the conversation the test forces.

**The Vite dev proxy rather than CORS in development.**
Restating unit 11's config line for why it belongs here. Without the proxy, the page is on
`:5173` and the API on `:8080` — two origins — so every `fetch` becomes cross-origin, and
because we send an `Authorization` header, **every one of them preflights**. That is an
extra round trip per request, CORS configuration that exists only in development, and a
dev/prod divergence in the one layer you least want it. The proxy makes development match
production: one origin, no headers, the same relative `fetch('/healthz')` in both.
*The rejected alternative* — a `VITE_API_BASE_URL` env var plus permissive CORS in dev —
is the common pattern and it is worse: it introduces a code path that only ever runs in
development, so the production path is the one that is never exercised until it breaks.

**Preflights are refused rather than handled.**
We register no `OPTIONS` handler, so a preflight lands on the `/api/` catch-all and gets a
404 with no CORS headers. A browser then never sends the real request. That is the correct
outcome: nothing should be calling this API cross-origin, and the fix for something that is
would be to stop splitting the origins, not to start approving them.

## Walkthrough

There is no production code in this unit. The behaviour being taught is the *absence* of
CORS, and the artefacts are the tests that keep it absent.

### `internal/server/routes_test.go`

`testServer` (`:22`) constructs a `Server` with a **nil store**. Every route these tests
touch — `/healthz`, the SPA, the API 404 — is reachable without the database, which is what
keeps this file fast and independent of Docker. Passing nil is safe here precisely because
none of these paths dereference it; anything that needs data belongs in `internal/store`.

`TestNoCORSHeaders` (`:51`) sends an `Origin` and both preflight request headers to five
different routes, then iterates over **every response header** looking for an
`access-control-` prefix. Checking for the prefix rather than for specific header names
matters: a middleware someone adds later might emit `Access-Control-Expose-Headers` or
`-Max-Age` without emitting `-Allow-Origin`, and a test that only looked for the obvious one
would let that through.

`TestPreflightIsNotApproved` (`:88`) states the same thing as behaviour rather than
absence, and its comment records *why* the status is 404 and not 405: `OPTIONS
/api/endpoints` matches only the `/api/` catch-all, so there is no handler for the method
to be mismatched against. Both are refusals from the browser's point of view.

`TestSPAFallback` and `TestCacheHeaders` skip when no frontend build is embedded (`:133`,
`:168`) rather than failing. A skip here is honest — the assertion genuinely cannot be
evaluated — and CI always has the build, because the frontend step runs before every Go
step. The alternative, failing, would mean `go test ./...` is red on a fresh clone for a
reason unrelated to the change being made.

`TestCacheHeaders` discovers the hashed asset filename by reading the embedded directory
(`:181`) rather than hardcoding `index-Cb3gDD6b.js`. That hash changes on every frontend
edit; a hardcoded one would make this test fail on unrelated CSS changes, which is how
tests get deleted.

## Verified

Six tests, all passing, plus a live demonstration against the running binary.

**The preflight a browser would send before any authenticated call:**

```
$ curl -i -X OPTIONS http://localhost:8080/api/endpoints \
    -H 'Origin: http://localhost:5173' \
    -H 'Access-Control-Request-Method: POST' \
    -H 'Access-Control-Request-Headers: authorization,content-type'

HTTP/1.1 404 Not Found
Content-Type: application/json; charset=utf-8
```

No `Access-Control-Allow-Origin`, so a browser stops there and the real request is never
sent.

**Access-Control-\* headers emitted across all five routes tested: 0.**

And the point that makes CORS click:

```
$ curl http://localhost:8080/healthz -H 'Origin: http://evil.example'
{"status":"ok"}
```

The server answered normally. It does not know what an `Origin` is and has no opinion about
it. A *browser* would have fetched that same 200, found no `Allow-Origin`, and refused to
let the page read it — while the server log shows nothing but a successful request. That
asymmetry is the entire subject: CORS is enforced on the client, after the response has
already been produced.
