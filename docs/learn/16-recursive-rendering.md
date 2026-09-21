# 16 — Recursive rendering: the JSON tree

*Phase 2, unit 6. Covers the curriculum bullet "recursive rendering (the JSON tree)", the
other half of the bullet begun in [15](15-server-state.md).*

## Brief

**What is it?** A component that calls itself to render nested data. The rendering mirrors
the shape of the data: one function handles a single node and invokes itself for that
node's children.

**What problem does it solve?** JSON nests arbitrarily deep. You cannot write a component
per level, because there is no last level — `{"a":{"b":{"c":...}}}` can go on as long as
the sender likes. And a loop cannot do it either: a loop is flat, and the data is a tree.

Before people reached for recursion here, the options were all lossy. Flatten to dotted
paths (`user.address.city`) and you lose structure. Render a `<pre>` of pretty-printed text
and you lose interaction — no collapsing, no clicking a value to copy it. Hand-write two or
three levels and give up below that.

Recursion is the only control structure whose shape matches the data's shape, which is why
the code ends up shorter than any of the alternatives.

**How does it work underneath?** React builds its element tree by calling functions. When a
`Node` component returns `<Node value={child}/>`, React calls `Node` again during
reconciliation — the recursion happens *during rendering*, not in a loop you write. Depth
in the data becomes depth in the element tree, and, while rendering, depth in the **call
stack**.

That last part is both the mechanism and the constraint, and it is the thing worth
carrying: a tree component's render is a recursive function call, so the data's depth is
your stack depth.

**Sharp edges.**

- **The stack is finite; the input is not.** Deeply nested JSON — a hostile payload, or
  something linked-list shaped — can exhaust it, and a stack overflow takes the whole tab
  down rather than showing an error. For an inspector accepting arbitrary bytes from
  strangers, a depth cap is a defence rather than a nicety.
- **Rendering everything is slow.** A ten-thousand-key object is ten thousand components.
  Collapsing past a depth by default, and *not rendering* collapsed children rather than
  hiding them with CSS, is the difference between usable and not.
- **Keys.** Siblings need stable keys. Array index is acceptable here only because a
  captured request never changes after it arrives — in a reorderable list it would be a bug.
- **JSON's types are easy to blur.** `null`, `false`, `0` and `""` all look "empty-ish" if
  you are careless, and rendering `false` identically to `"false"` hides exactly the sort of
  bug someone opened an inspector to find. Type has to be visible.
- **The payload may not be JSON at all.** Ours is arbitrary bytes — protobuf, gzip,
  form-encoded, or malformed JSON, which is very often the thing being debugged. A tree is
  one view; a raw view must always be reachable, and the fallback has to be graceful rather
  than an error screen.

**In hooklens.** The tree is the centrepiece — the thing someone stares at all day. It gets
a depth cap, collapse-by-default past the first couple of levels, type-distinct rendering,
and a raw view for anything that will not parse, including bodies that are not text at all.

## Decisions

**A `Node` that dispatches on type, not a component per type chosen by the caller.**
The alternative is `<JsonObject>`, `<JsonArray>`, `<JsonString>` and a caller that picks.
That pushes the type test to every call site — and every call site is *inside* the
components themselves, so the test gets duplicated once per container type and drifts.
One `Node` with the dispatch in one place means adding a type (a date, a detected base64
blob) is a single edit.

**A depth cap of 64, rendering a message instead of the subtree.**
This is the decision the brief was pointing at the whole time. React's recursion happens
during rendering, so *the data's depth is the call stack's depth* — and the data is
supplied by strangers. Without a cap, a payload nested ten thousand deep overflows the
stack, and a stack overflow in React does not show an error boundary, it takes the tab
down. 64 is far past anything a real webhook does (Stripe's deepest is about 6) and far
short of the limit. Rejected: a `try/catch` around the render, because a stack overflow
cannot be caught reliably, and by the time it could be the render is already
half-committed.
*The reason this costs nothing:* the raw tab always shows the complete bytes, so the cap
hides nothing — it redirects.

**Collapse below depth 2, and collapsed children are absent rather than hidden.**
Two separate things. The depth-2 default is so a 400-key payload opens showing its shape
instead of four screens of scrolling. That children are *not rendered* rather than
`display: none` is the load-bearing half: hiding with CSS costs exactly as much rendering
as showing, so a collapse that only hides solves the scrolling and leaves the performance
problem untouched.
*Wrong call if* we wanted Ctrl+F to find text in collapsed branches — it cannot, because
that text is not in the DOM. Accepted deliberately; a search box over the parsed value is
the better answer there, and it is a Phase 4 concern.

**`<Detail key={selected}>` — remounting on selection, not updating.**
Without the key React reuses the component, and component state outlives the data it
described: the tab you were on and every node you expanded carry over onto the next
capture. That is not a cosmetic glitch in this app — expanded branches drawn over a
different request's data is *the tool lying about what arrived*. The key makes selection a
mount, which throws that state away by construction rather than by remembering to reset it
in an effect.

**Index as the key for headers, name as the key for object entries.**
Deliberately different, for the same reason. Object keys are unique, so the name is the
identity. Header names are *not* unique — `Set-Cookie` repeats, and signature schemes send
several candidates — so keying by name would collapse duplicates. Losing a duplicated
header is precisely the failure the Phase 1 `[]Header` decision existed to prevent, and it
would have reappeared here in the view layer.

**Array indices as keys inside the tree.**
Normally a bug. Safe here for a specific reason worth stating rather than assuming: a
captured request is immutable, so the array never reorders, never has an item inserted,
and never has one removed. The moment any of that becomes false, this is wrong.

**JSON tags added to `capture.Header`.**
The detail endpoint was emitting `{"Name","Value"}` — the only capitalised keys in an
otherwise snake_case API — because the struct had no tags. Safe to fix now because the
store marshals through its own `headerJSON` DTO, so the stored encoding is decoupled from
the wire encoding and rows already in Postgres are unaffected. That DTO looked like
duplication when it was written; this is the payoff.

**`staleTime: Infinity` on the detail query, for a stronger reason than on the list.**
On the list it was a tuning choice justified by the stream. Here it is a statement about
the data: a captured request is immutable, so a cached copy *cannot* go stale and a
refetch could only ever return the same bytes.

**Decoding and classifying the body is memoised on the base64 string.**
Up to a megabyte of base64 decode plus a UTF-8 validation pass, otherwise re-run on every
render — and switching tabs is a render. `useMemo` keyed on the encoded body, which
changes exactly when the capture does.

**The raw tab reconstructs the request rather than storing it.**
The literal wire bytes are not kept: Go has already parsed the request line and normalised
header casing by the time we see it, and keeping a second copy of every payload to serve
this one view is not worth the storage. What is shown is the recognisable shape — request
line, headers, blank line, body — which is what a person pastes into a bug report.
*Worth knowing it is a reconstruction*, because it means this view is not evidence of
byte-exactness. When Phase 4 needs byte-exactness for HMAC, the answer is the stored body,
not this pane.

## Walkthrough

### `web/src/JsonTree.tsx`

`MAX_DEPTH` (`:19`) and `AUTO_COLLAPSE_DEPTH` (`:27`) are two different ideas that look
alike. The first is about not crashing, the second about not rendering what nobody asked
for. The comments say which is which, because a later reader tempted to fold them into one
constant would silently remove the stack guard.

`Node` (`:37`) is the recursion. The depth check comes **first**, before any type test — a
guard placed after the dispatch is one the object and array branches can skip past. Then
primitives, then arrays, then objects, then a fallback. The ordering below the guard is not
load-bearing except that `Array.isArray` must precede `typeof === 'object'`, because arrays
*are* objects and the object branch would happily render `[1,2]` as `{0: 1, 1: 2}`.

The `depth + 1` at `:59` and `:70` is the entire recursive step. Pass `depth` unchanged and
the cap never fires.

`Branch` (`:81`) owns the collapsed state, which means **each node owns its own**. State
lives at the node, so expanding one branch does not disturb the others, and there is no
central map of paths to keep in sync with the tree. It is also why the `key` on `Detail`
matters: this state is spread across hundreds of components and there is nowhere to reset
it from.

`count === 0` returns a `Leaf` (`:100`) so `{}` and `[]` render as text rather than as a
toggle that expands to nothing.

The `{!collapsed && ...}` at `:127` is the decision made concrete: children sit inside the
conditional, so collapsing removes them from the tree rather than hiding them.

### `web/src/Detail.tsx`

The tab state is a single `Tab` union rather than three booleans, so "two tabs active at
once" is not representable.

`Body` (`:65`) does its work through `classifyBody` from the pure module built earlier in
this unit, so the branching is testable under `node --test` with no browser. The `switch`
has no `default`: with a discriminated union, TypeScript proves the four cases exhaustive,
and adding a fifth `BodyView` kind becomes a compile error here rather than a blank pane at
runtime.

The `text` case (`:78`) matters most and looks least interesting. Non-JSON very often means
*malformed* JSON, and malformed JSON is frequently the bug being hunted — so it renders the
bytes verbatim instead of an error.

`Headers` (`:96`) — see the decision on the index key. This is where the Phase 1 choice to
store `[]Header` instead of a map becomes visible to the user.

`Raw` (`:117`) reconstructs; see the decision on why, and on what that means for Phase 4.

### `web/src/App.tsx`

`selected` holds an **id**, not a capture object (`:15`). The list is a cache the stream
rewrites; a copy of a row held here would be a second source of truth for the same capture,
and the two would diverge the moment anything updated one.

`key={selected}` on `<Detail>` (`:119`) — the remount. Verified below.

`forget` clears the selection before clearing the inbox, so the detail pane cannot outlive
the token it needs in order to fetch.

### `web/src/index.css`

`.panes` uses `minmax(0, 1fr)` rather than `1fr`. A grid track's default minimum is
`min-content`, so one very long header value or unbroken JSON string would push the track
wider than the viewport and scroll the entire page sideways.

`.t-children` draws its indent with a left border, so the guide line is produced by the
same rule that creates the offset — nesting stays visible without a per-depth style or a
padding computed in JavaScript.

The row is a `<button>` (`.rowbtn`), not a click handler on the `<li>`. A handler on the
list item would be unreachable by keyboard and invisible to a screen reader; a button is
focusable, activates on Enter and Space, and announces itself, for free.

## Verified

Driven in Chrome against the running binary, with six captures chosen to hit every branch.

**Every body kind, each rendering as its own case:**

| capture | body | rendered as |
|---|---|---|
| `POST /stripe` | 418 B nested JSON | interactive tree |
| `POST /broken` | 44 B, trailing comma | verbatim text, 44 chars, **no error** |
| `POST /upload` | 26 B PNG header | binary notice + hex dump with offsets |
| `GET /ping` | 0 B | "no body" |
| `POST /form` | 22 B form-encoded | verbatim text, 22 chars |

**Collapse-by-default.** The Stripe payload opened with `data` expanded and `data.object`
collapsed reading `{ 5 keys }` — depth 2, as configured. Expanding it revealed `charges`
and `metadata`, each collapsed in turn.

**Duplicate headers survive to the screen.** Two `Stripe-Signature` headers were sent and
both appear, in order, in the headers tab and the raw view:

```
Stripe-Signature	t=1,v1=abc
Stripe-Signature	t=1,v1=def
```

A `map[string]string` anywhere in the chain — storage, API, or this table's key — would
have shown one. This is the Phase 1 decision paying out, three phases later.

**The remount is real.** Instrumented rather than eyeballed: opened `/stripe` (body tab, 3
nodes expanded), expanded everything and switched to the raw tab, selected `/form`, then
selected `/stripe` again:

```
fresh               -> body:3 expanded
after expanding all -> body:4 expanded
after raw tab       -> raw:0 expanded
switched away       -> body:0 expanded
back to stripe      -> body:3 expanded
```

Back at the fresh default, not the raw tab with everything open. Remove the `key` and the
last line reads `raw:` with the expansions intact.

**The depth cap holds.** Sent 300 levels of `{"a":` nesting (1808 bytes). Expanding
repeatedly stopped at **65 rendered levels with exactly one cap message**, page responsive,
no console errors — rather than a blown stack. The raw tab for the same capture still
showed all 1808 bytes, with 300 opening and 300 closing braces, confirming the cap
redirects rather than hides.

Bundle 264 KB raw, **82 KB gzipped** — up 2 KB on unit 15.

### A tooling note

The extension's element-reference clicks reported success but dispatched no event the page
saw; driving the same elements with a real `.click()` worked immediately. Same family as
unit 15's screenshot timeouts on this machine. Recorded so the next person checks the
server log first — it showed zero `/api/requests/{id}` hits, which proved the click and not
the handler was at fault — before suspecting their own code.

Twice, `Runtime.evaluate` returned `[BLOCKED: Cookie/query string data]`: the extension
filters returned text that looks like a query string, and both `probe=1` and
`name=ada&role=engineer` qualify. Returning structured counts and booleans instead of raw
body text sidesteps it.
