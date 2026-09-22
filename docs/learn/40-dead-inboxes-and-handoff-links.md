# 40 — Knowing when to stop retrying, and handing a secret across a URL

> Phase 6 · unit 40. Two gaps found by using the app rather than testing it: a browser
> that retried a deleted inbox forever, and a CLI whose inbox could not be opened in
> the browser at all.

## Brief

**What is this thing?** Two small mechanisms. The first is **error classification in
a retry loop**: deciding, for each failure, whether trying again could ever succeed.
The second is the **URL fragment as a client-only channel**: the part after `#`, which
the browser keeps to itself and never sends to any server.

**What problem does each solve?** A retry loop exists for failures that go away — a
server restarting, a network blip. It is actively harmful for failures that do not:
retrying a rejected credential forever wastes requests, looks like a guessing attack
from the server's side, and — worst — tells the user "retrying…", which promises a
recovery that will never come. The CLI learned this in [22](22-backoff-and-jitter.md)
(`CloseError.Permanent()`); the browser's stream loop never did, so an inbox that no
longer existed showed "retrying" and a red "missing or invalid token" indefinitely.

The second problem is handing the CLI's inbox to the browser. The CLI holds a slug and
a token; the browser needs both, and the token is a secret. The options were all bad
in known ways: a query string lands in server access logs, proxy logs and the
`Referer` header ([08](08-capability-urls.md) refused exactly that); asking the user to
copy two values by hand is where people give up.

**How does it work underneath?** For classification, the signal is the HTTP status:
`401`/`403` mean *this credential is not accepted*, which no amount of waiting changes;
a network error, `502`, `503` or a dropped stream may heal. Permanent failures stop
the loop and change what the UI says; transient ones back off and retry.

For the fragment: a browser parses `https://host/path#frag` and sends only
`GET /path` — the fragment is not part of the HTTP request at all, and browsers strip
it from `Referer`. So a token in the fragment reaches the page's JavaScript and
nothing else. The page then reads it, stores it, and removes it from the address bar
with `history.replaceState`, which rewrites the current history entry rather than
adding one.

**What are the sharp edges?**

- **Misclassifying a transient error as permanent** is worse than the original bug: a
  server restart would log everyone out. Only statuses that *mean* "rejected" count.
- **The fragment is not invisible.** It is in the terminal's scrollback, in the
  browser's address bar until the page strips it, and possibly in history if the page
  never runs. It keeps the secret away from *servers*, not from the machine.
- **A link can overwrite something the user cannot get back.** The browser remembers
  one inbox, and a token cannot be recovered from the server. A link that silently
  replaced it would destroy access to the old inbox.

**In hooklens:** the stream stops on `401`/`403` and the page offers a new inbox
instead of "retrying"; `hooklens forward` prints an `inspect` link carrying its inbox
in the fragment; and the page asks before a link replaces a different inbox.

---

## Decisions

**Only 401 and 403 stop the stream.**
The tempting version stops on any 4xx. Rejected: a 429 (rate limit) and a 404 from a
proxy mid-deploy both heal, and stopping on them would strand every open tab. The
narrow rule matches the CLI's `CloseError.Permanent()` from
[22](22-backoff-and-jitter.md): only "your credential is not accepted" is permanent.
**Wrong if** the server ever used 401 for something transient, like an auth backend
being briefly down. It doesn't; auth is a local hash compare.

**"Gone" is checked on both the stream and the list fetch.**
Either can be first to learn the inbox is gone, and before this the list fetch showed
a red "missing or invalid token" while the stream kept saying "retrying". The page now
shows one clear state, *inbox not found*, with a button to start a new inbox, and
hides the capture section whose `curl` hint pointed at an inbox that can no longer
receive anything.

**The handoff link uses the fragment, not a query string or a one-time code.**
A query string puts the token in access logs and `Referer` headers, which is the leak
[08](08-capability-urls.md) refused. A one-time exchange code (the CLI asks the server
for a short-lived code, the browser trades it for the token) is the stronger design:
nothing long-lived ever appears in a URL. Rejected for now because it needs a new
server endpoint, a code store and an expiry, all for a local developer tool whose
token already sits in the same terminal session. **The signal to switch** is
hooklens being hosted, where "inspect" links might be pasted into chat or tickets.

**The page asks before a link replaces a different inbox.**
The browser remembers one inbox, and the server only keeps a token's hash, so a
silently replaced inbox is lost for good. Same inbox, or nothing stored: adopt
without asking. A different one: a banner asks, and "keep this one" leaves
everything as it was. **Wrong if** the browser later remembers several inboxes; then
the link just adds one and there is nothing to ask.

**The fragment is stripped with `replaceState`, on load *and* on `hashchange`.**
`replaceState` rewrites the current history entry instead of adding one, so the token
doesn't linger on screen or in a copied URL. The `hashchange` listener was a
correction: opening the link in a tab where hooklens was already open changes only
the `#` part, which browsers treat as a jump within the page, so the first version
(read once on load) never saw it and left the token in the address bar. Found by
doing exactly that during manual verification.

---

## Walkthrough

- **`web/src/lib/sse.ts:50`**: `isPermanentStatus`, the classification in one
  place, with its reasoning. `:149` is where the loop uses it: set `closed`, call
  `onFatal`, and return, so there's no backoff sleep and no further requests.
- **`web/src/lib/handoff.ts`**: pure functions, no React, so they run under
  `node --test`.
  - `inboxFromHash` (`:22`) validates the slug with the server's own rule, so a
    malformed link is ignored instead of stored and then rejected as a mystery
    "invalid token".
  - `hashHasToken` (`:33`) is separate, so even a *malformed* link gets its token
    stripped from the address bar.
  - `handoffDecision` (`:48`) is the adopt / ask / none rule.
- **`web/src/lib/useInbox.ts:44`**: `inboxRef`, so the `hashchange` listener compares
  against the inbox stored *now*, not the one stored when the listener was attached
  (a stale closure). `:74` subscribes and cleans up.
- **`web/src/App.tsx`**:
  - `:63` defines `gone` from both sources.
  - `:91` is the ask-first banner.
  - `:117` is the not-found section.
  - `:145` hides the capture section while the inbox is gone. Hidden rather than
    unmounted, so its hooks and state stay stable.
- **`cmd/hooklens/forward.go:264`**: `inspectLink`. `url.Values.Encode` handles the
  escaping; `TrimRight` avoids a double slash if `--server` ends in `/`.

---

## Verified

- 8 new frontend tests (`web/src/lib/handoff.test.ts`), including one that decodes
  exactly what the CLI encodes (`=`, `+`, `/` percent-encoded). 1 new Go test
  (`cmd/hooklens/forward_link_test.go`), which checks the token is in the fragment
  and never the query string. Frontend 42/42, Go suite green, lint 0 issues.
- **Manually, on a real server** (a copy of the new build on port 8090, leaving the
  author's own running instance alone):
  1. A browser holding a non-existent inbox showed **inbox not found** with a
     *Create a new inbox* button. Status `gone`, and exactly **one** stream request
     instead of a retry loop.
  2. `hooklens forward --server http://localhost:8090 --new` printed an inspect link
     with the inbox in the fragment.
  3. Opening that link in the already-open tab **failed** with the first version:
     no banner, token left in the address bar. Fixed with `hashchange`, rebuilt,
     retried: the address bar was cleaned at once, and the banner asked before
     replacing the stored inbox.
  4. After accepting: status `live`, and a webhook sent to the tunnel's inbox
     appeared in the list.
- **One testing trap, recorded so it isn't mistaken for a bug:** a click sent by the
  browser automation to a *hidden* window did nothing. The same click from inside the
  page worked. The first failed attempt at step 4 was the test harness, not the app.
