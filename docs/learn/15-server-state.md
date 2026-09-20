# 15 — Server state, and TanStack Query's cache model

*Phase 2, unit 5. Covers the curriculum bullet "React state and effects; TanStack Query's
cache model". Recursive rendering, the other half of that bullet, is
[16](16-recursive-rendering.md).*

## Brief

**What is it?** A cache for server state, keyed by a query key, that handles fetching,
sharing, staleness and invalidation on your behalf.

**What problem does it solve?** There are two kinds of state in a frontend, and conflating
them is the original sin.

**Client state** is owned by the UI — which pane is open, what is typed in a filter box. It
is synchronous, always correct, and nobody else can change it. `useState` is exactly right
for it.

**Server state** is a *copy* of something that lives elsewhere. It can be stale the moment
it arrives, someone else can change it without telling you, and it needs loading and error
states. It is not really state at all — it is a cache, and pretending otherwise is where
the trouble starts.

The default pattern treats the second like the first:

```
const [data, setData] = useState(null)
const [loading, setLoading] = useState(true)
useEffect(() => { fetch(...).then(setData) }, [])
```

That looks fine and rots predictably. Two components wanting the same data fetch it twice.
Unmounting mid-flight sets state on a dead component. There is no shared invalidation after
a mutation, no retry, no refetch when the tab regains focus, no deduplication. Each gap is
a few lines; together they are a library, written badly, once per component.

**How does it work underneath?** A global cache keyed by a **query key** — an array like
`['requests', slug]`. `useQuery` looks the key up and returns whatever is cached
immediately, even if stale, then refetches in the background and re-renders when the new
data lands. Every mounted `useQuery` sharing a key is a subscriber to that cache entry, so
a single write re-renders all of them, and a single fetch serves all of them.

Two knobs people routinely confuse. **`staleTime`** is how long data counts as fresh —
within it, no background refetch happens. **`gcTime`** is how long an *unused* entry
survives in memory after its last subscriber unmounts. The default `staleTime` is `0`,
meaning every mount refetches, which surprises nearly everyone.

Changing data has two routes. `invalidateQueries` marks entries stale and refetches the
mounted ones — the normal answer. But `setQueryData` writes into the cache *directly*, and
that is the one that matters here: we already have the new row arriving over SSE, so
refetching the entire list to learn what we were just told would be a wasted round trip.

**Sharp edges.**

- **The query key *is* the cache identity.** Omit a variable from it and two logically
  different queries share one entry — inbox A's captures rendered under inbox B. It fails
  silently and looks like a backend bug.
- **`staleTime: 0` by default.** Remount means refetch. Often fine, frequently surprising.
- **Writing to the cache by hand can lie.** `setQueryData` puts whatever you hand it into
  the cache. If the server disagrees, the UI shows something untrue until the next refetch.
  That is acceptable for an event the server just sent us; it is much riskier as an
  optimistic guess.
- **Effects run twice in StrictMode** in development, deliberately, to expose missing
  cleanup. A subscription without a teardown shows up as doubled data.

**In hooklens.** The capture list is server state; the selected request id is client state.
The SSE stream writes new captures straight into the cached list rather than triggering a
refetch — one round trip saved per webhook, and the row appears the instant the event
arrives rather than after a round trip that tells us what we already knew.

## Decisions

**TanStack Query for the capture list, `useState` for the inbox.**
The split is the brief's distinction made concrete. The inbox — slug and token — lives only
in this browser, nothing else can change it, and it is never fetched. That is client state
and `useState` is right. The capture list is a copy of rows in Postgres that anything can
add to at any moment. That is a cache, and hand-rolling it means hand-rolling
deduplication, cancellation on unmount, retry and invalidation, badly, in every component
that needs it.
*Wrong call if* the app stayed at one query. It did not — unit 16 adds a second for the
request detail, and the two share the token, the error shape and the loading conventions.

**`setQueryData` on a capture event, not `invalidateQueries`.**
This is the unit's real decision. Invalidating is the reflex, and here it would refetch the
entire list *to learn what the event just told us* — a round trip per webhook and a visible
delay before the row appears. Writing into the cache directly is safe in a way an
optimistic update is not: this is not a guess about what the server will do, it is the
server reporting what it already did.
*Wrong call if* the event were lossy or partial. Ours carries exactly the fields the list
renders, which is why the summary shape in unit 14 was chosen to match.

**`staleTime: Infinity` and `refetchOnWindowFocus: false`.**
Both defaults exist for apps where the server changes behind your back and polling-on-focus
is the cheapest way to notice. We have a live stream doing precisely that job, so the
defaults would add background refetches that can only ever confirm what SSE already
delivered. The initial load is the one fetch we actually need.
*The signal this is wrong:* if the list ever diverges from the server, it means an event
was missed — which is what the drop counter reports, and the answer there is a deliberate
resync, not ambient polling.

**Duplicate guard on insert, even though the server does not resend.**
`if (prev.requests.some(r => r.id === capture.id)) return prev`. Two real races make this
necessary rather than defensive: a stream reconnect can replay, and the initial fetch can
land *after* an event for a row it already contains. Without the guard the same capture
appears twice — which, in a tool whose entire purpose is answering "why did this fire
twice", is the single most damaging bug available.

**A `keys` factory rather than inline query keys.**
The key *is* the cache identity. Built inline in two places, they drift by one variable and
two inboxes silently share an entry — inbox A's captures rendered under inbox B, looking
exactly like a backend bug. One factory makes that impossible to get wrong.

**`retry: 1` globally.**
This is a local debugging tool. A server that is not answering is usually one the developer
just stopped, and three retries with backoff only delays the error that tells them so.

**`ApiError` declares its field instead of using a constructor parameter property.**
`constructor(readonly status: number)` is the idiomatic TypeScript and it does not compile
here: it emits runtime assignments, so it is not *erasable* type syntax, and this project
has `erasableSyntaxOnly` on. That flag is what lets Node run these files directly by
stripping types with no transform step — which is what makes the zero-dependency test setup
from unit 13 possible. A small tax for a real benefit.

**`localStorage` writes are wrapped in try/catch.**
Private mode, blocked site data and quota all throw. The app still works without
persistence — it just forgets the inbox on refresh — so degrading beats failing.

## Walkthrough

### `web/src/lib/api.ts`

Everything that talks to the server lives here, so no component builds a URL or has to
remember the token.

`get` (`:36`) parses the error body defensively even though our server always answers JSON,
including on a 404 — that is what the `/api/` catch-all from unit 11 guarantees. The
defensiveness is for a proxy that might one day sit in front and return HTML.

`keys` (`:81`) is the query-key factory. See the decision above for why it is not inline.

### `web/src/lib/useInbox.ts`

Three hooks, one per kind of state.

`useStoredInbox` (`:14`) is client state with persistence. The lazy `useState` initialiser
reads `localStorage` once on mount rather than on every render.

`useRequests` (`:49`) puts `slug` in the query key, so switching inboxes reads a *different*
cache entry rather than briefly showing the previous inbox's captures while a refetch runs.

`useLiveCaptures` (`:74`) is where the stream and the cache meet. The effect returns
`streamEvents`' own close function directly (`:90`), which is the cleanup — without it,
StrictMode's deliberate double-mount leaves an orphan connection feeding the same cache and
every capture arrives twice.

The `setQueryData` updater (`:112`) returns `prev` unchanged when there is no cache entry
yet: an event arriving before the initial fetch completes needs no handling, because that
fetch will include the row anyway. Returning a fabricated one-item page instead would show
a list missing everything older.

### `web/src/main.tsx`

The `QueryClient` is constructed at module scope, outside the component tree. Created
inside a component it would be replaced on every render, throwing the cache away each time
— a bug that presents as "the app refetches constantly and nothing is ever cached".

## Verified

Driven in a real browser, not asserted.

Created an inbox through the UI, confirmed the stream indicator read **live**, then sent
five webhooks from a terminal while the page sat open. All five appeared **without a
refresh**, newest first:

```
CAPTURES (5)
DELETE  /thing              0 B   01:45:55
PUT     /orders/42?force=1  3 B   01:45:55
POST    /webhook           34 B   01:45:54
POST    /webhook           34 B   01:45:54
POST    /webhook           34 B   01:45:54
```

Method, path, query string, body size and time, in arrival order, with no console errors.
Three identical POSTs rendered as three rows rather than being deduplicated — correct: they
are distinct captures with distinct ids, and collapsing them would hide exactly the
duplicate-delivery behaviour this tool exists to reveal.

Bundle 258 KB raw, **80 KB gzipped** — up about 10 KB for TanStack Query.

### A tooling note

`Page.captureScreenshot` timed out repeatedly against this tab, which looked at first like
the page had frozen. It had not: text extraction worked throughout and the console was
clean. Navigating to `/healthz` — plain JSON, no React, no stream — and screenshotting
*that* also timed out, which rules the application out entirely. A CDP quirk on this
machine, recorded so the next person does not spend the same ten minutes suspecting their
own code.
