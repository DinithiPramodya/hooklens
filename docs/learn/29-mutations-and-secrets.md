# 29 — Mutations, and holding a secret in a browser

*Phase 4, unit 4. The UI for verification, replay and diff. Two ideas worth teaching: why
TanStack Query separates mutations from queries, and where a user's webhook secret is
allowed to live.*

## Brief

**What is this thing?** The three Phase 4 features made usable: a signature panel that
takes a secret and shows what was compared, a replay button, and a diff view between two
captures.

**What problem does it exist to solve?** All three already work over HTTP. What is missing
is that they are *actions* — the first things in this app that are not reads — and the
state model built in [15](15-server-state.md) was for reads.

**How does it actually work underneath?** A query and a mutation differ in four ways, and
the library separates them because every one of those differences would otherwise be
hand-rolled at each call site.

A **query** is keyed, cached, deduplicated, and **runs on its own** — mount the component
and it fetches. It is a declaration: *this data should be here*. A **mutation** has no key,
is not cached, never runs by itself, and is triggered by a person. It is an instruction:
*do this now*.

The consequences are practical. A query that runs twice is free — the second call hits the
cache. A mutation that runs twice sends two requests, and for a replay that means the
developer's app is hit twice, which is a real event that cannot be undone. So a mutation
carries `isPending` and the UI disables the button, where a query carries `isLoading` and
the UI shows a spinner.

The other half is the secret. A webhook signing secret is a **bearer credential for
forging events** — anyone holding it can send the developer's own application a payload it
will believe. Once it is typed into a browser, the question is where it goes.

`localStorage` persists across tabs, restarts and — on a shared machine — users, and it is
readable by any script that gets injected into the origin. `sessionStorage` is narrower but
still survives reloads. A React `useState` lives in memory for as long as the tab does and
disappears on reload, which is mildly annoying and is the correct trade for something this
sensitive.

**What are the sharp edges?**

- **`autoComplete="off"` is ignored by password managers.** A field that should never be
  saved needs `type="password"` plus `autoComplete="new-password"`, and even that is a
  request rather than a guarantee.
- **A secret in component state is still in a React DevTools tree**, and in a heap
  snapshot. Memory-only is better than storage, not the same as safe.
- **Mutations invalidate nothing by default.** If an action changes server state — a replay
  creates a new capture — the list must be told, or the UI silently shows stale data.
- **A double-click sends two requests** unless the button is disabled while pending.

**In hooklens:** the secret lives in `useState` and nowhere else, the field is
`type="password"`, the panel says the secret is not stored, and every action is a
`useMutation` with its trigger disabled while pending.

## Decisions

**The secret lives in `useState` and nowhere else.**
A signing secret is a bearer credential for **forging events**: anyone holding it can send
the developer's own application a payload it will believe. `localStorage` persists across
restarts and across users of a shared machine, and is readable by any script that gets
injected into the origin; `sessionStorage` is narrower and still survives reloads.
Component state lives in memory for as long as the tab does and is gone on refresh.
Re-typing it is mildly annoying and is the right trade.
*Wrong call if* users end up re-entering it dozens of times a session and start pasting it
somewhere worse to avoid the friction. The signal would be people asking for it to be
remembered; the answer then is a session-scoped store with an explicit opt-in, not a
silent one.

**`autoComplete="new-password"`, not `"off"`.**
`off` is widely ignored by password managers. `new-password` is the value they actually
respect — and it is still a request rather than a guarantee, which is worth knowing rather
than assuming.

**The panel says the secret is not stored, in the UI.**
A claim in a doc comment protects nobody. The person typing it is the person who needs to
know.

**Verify and replay are mutations; diff is a query.**
Not bookkeeping. A query is keyed, cached, deduplicated and runs on mount — it declares
*this data should be here*. A mutation has no key, never runs by itself, and is an
instruction. A query running twice is free; a **replay** running twice hits the
developer's app twice, which is a real event that cannot be undone. Hence `isPending` and
a disabled button on both mutations.
Diff is a read of two immutable captures, so it is a query with `staleTime: Infinity`.

**A tunnel replay invalidates the capture list.**
Mutations invalidate nothing by default, and a replay through the tunnel creates a **new
capture** server-side. Without the `invalidateQueries` the list silently shows stale data
— the half of the query/mutation split that is easiest to forget, because everything still
appears to work.

**The edited body is only sent when it was actually touched.**
`edited === null` means untouched, and the field is omitted. Sending the unchanged body
would mark every replay as edited and print the "your signature will not match" warning on
replays where the signature is fine — training people to ignore it.

**The body editor seeds itself on first open, not on mount.**
Opening and closing the editor without typing must not count as an edit.

**Shift-click to compare, rather than a compare mode.**
Selecting two things with a modifier is already a familiar gesture from every file
browser. A mode toggle would be one more piece of state to explain and to get out of.

**The canonical-string section auto-opens on failure and stays closed on success.**
`<details open={!r.valid}>`. When it passed, the string is available and not in the way;
when it failed, it is the thing being looked for.

**`safeAtob`.**
`atob` throws on invalid base64 and produces mojibake for bytes outside Latin-1, and a
captured body is arbitrary bytes. Both are reachable here, so it degrades to a message
rather than taking the pane down.

## Walkthrough

### `web/src/Signature.tsx`

`useState` for the secret (`:22`) with the reasoning inline, `type="password"` and
`autoComplete="new-password"` (`:43`), and the disabled-while-pending button (`:55`).

`VerifyPanel` (`:76`) renders three cases: not detected, valid, invalid. The `<details>`
(`:102`) is the point of the whole panel.

### `web/src/Replay.tsx`

`onSuccess` (`:28`) invalidates the list for a tunnel replay.

The `edited === null` guard (`:25`) and the `onToggle` seeding (`:79`).

### `web/src/Diff.tsx`

A `useQuery` (`:26`), unlike its two neighbours, with the reason in the doc comment.

The volatile toggle (`:68`) is the UI half of the decision from
[28](28-structural-diff.md): hide the noise by default, say that it is hidden, make it one
click to see.

### `web/src/App.tsx`

`compareWith` (`:20`) and the shift-click handler.

## Verified

The frontend suite stays at 29 tests; these three components are thin over logic already
covered by `internal/signature`, `internal/replay` and `internal/diff`, which have 75
tests between them. Testing the wiring belonged in a browser, so that is where it was
checked.

**Driven in Chrome against the running binary**, with two Stripe-signed payloads differing
only in `amount`, and `PLAN.md`'s three Phase 4 criteria one at a time:

*"the signature panel reads VALID"* — with the real secret:

```
panelClass = "delivery ok"
VALID · stripe · signed 0s before it arrived
```

and the input reported `type=password`.

*"corrupting the secret reads INVALID and shows the string it compared"*:

```
panelClass   = "delivery fail"
INVALID · stripe · signed 0s before it arrived
autoOpened   = true          <- the details section opened itself
showsSignedString = true     <- the full `t.body` canonical string
showsBothTags     = 2        <- expected and provided, side by side
```

*"diffing two payments highlights only the changed amount"* — shift-clicking the second
capture:

```
headings: ["body (2)"]
data.object.amount | changed | 9900 → 2500
id                 | changed | "evt_9900" → "evt_2500"
```

Two changes, both real — the fixture varies the event id as well as the amount. **No
headers section and no request-line section**, because the signature and `Date` headers
were filtered as volatile and the two requests went to the same path. That is the
filtering decision from unit 28 doing visible work: without it this diff would have shown
four header changes nobody asked about.

No console errors. Bundle 277 KB raw, **85 KB gzipped** — up 2 KB for three components.
