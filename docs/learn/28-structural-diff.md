# 28 — Structural diff: comparing trees, not text

*Phase 4, unit 3. Diffing two captured requests — nested JSON plus headers — so that a
developer can see what actually differs between the payment that worked and the one that
did not.*

## Brief

**What is this thing?** A comparison of two captured requests that reports differences as
**paths into the data** — `data.object.amount: 2500 → 9900` — rather than as changed
lines of text.

**What problem does it exist to solve?** The question "why did this one fail?" is almost
always answered by "what is different about it", and the two payloads are usually ninety-
nine percent identical. Finding the one field that changed by eye, in a 400-line JSON
document, is the task this replaces.

The obvious approach is a text diff: pretty-print both and run the same algorithm `git`
uses. It does not work, for reasons that are properties of JSON rather than of the
algorithm:

- **Object keys have no order.** Two payloads that are semantically identical can
  serialise with keys in different orders, and a line diff reports every one of them as a
  change. The signal drowns.
- **Whitespace and indentation are not data**, but they are lines.
- **A line is the wrong unit.** A change deep inside a long line shows as the whole line
  replaced, and a change that shifts indentation — adding one nesting level — shows as
  every descendant line changing.

**How does it actually work underneath?** Walk both documents in parallel from the root,
comparing by *path* rather than by position.

At each node: if the types differ, that is a change. If both are objects, take the union
of the keys — keys only in the left are **removed**, only in the right are **added**, in
both are compared recursively. If both are scalars, compare the values. The output is a
flat list of paths with an operation and the old and new values.

Sorting the key union matters, and not for looks: the same two documents must produce the
same diff every time, and Go's map iteration is deliberately randomised. Without sorting,
the output reorders between runs and becomes impossible to compare or test.

**Arrays are the hard part, and there is no correct answer.** Two options:

**By index** — compare `[0]` with `[0]`. Simple, and catastrophic for an insertion: add an
element at the front and every subsequent element reports as changed.

**By identity** — match elements that look like the same thing, usually by an `id` field,
then diff the matched pairs. Far better output when it applies, and it needs a heuristic
about which field identifies an element, which is a guess that can be wrong.

**What are the sharp edges?**

- **Numbers.** JSON has one numeric type and `encoding/json` decodes into `float64`, so
  `2500` and `2500.0` are indistinguishable after parsing — and large integers lose
  precision above 2^53. Comparing the raw tokens avoids both.
- **Null versus absent** are different states and must not render identically.
- **Headers are not JSON**: they are an ordered list with duplicates allowed, so they need
  their own comparison rather than being coerced into an object.
- **A diff of two non-JSON bodies** still has to do something useful.

**In hooklens:** an `internal/diff` package walks two decoded bodies and produces a sorted
list of changes, matching array elements by `id` when every element has one and falling
back to index when they do not.

## Decisions

**`json.Decoder.UseNumber()`, not the default float64 decoding.**
The default makes `2500` and `2500.0` indistinguishable and silently loses precision above
2^53 — so two ids differing in the last digit would compare **equal**, which is the worst
possible failure for a diff tool. `json.Number` keeps the original token, which is what a
diff should be comparing anyway: this tool shows what was on the wire.
There is a test with `9007199254740993` and `9007199254740992` that fails under float64
decoding.

**Values are stored and compared as encoded JSON, not as `any`.**
`"2500"` and `2500` print identically through a naive formatter, and telling a string from
a number is frequently the whole point. Encoding also keeps `null` distinguishable from
absent, and makes the output directly renderable.

**The key union is sorted, and that is correctness rather than tidiness.**
Go randomises map iteration deliberately. Without the sort, the same two documents produce
a differently ordered diff on every run — untestable, and impossible to compare against a
previous run by eye. `TestOutputIsDeterministic` runs the same comparison fifty times.

**Arrays: match by id when every element has a unique one, otherwise by index.**
The fork with no correct answer. Index matching is simple and catastrophic for an
insertion — add one element at the front and every subsequent element reports as changed,
which is technically true and useless. Id matching gives the answer a person wanted and
rests on a heuristic that can be wrong.
The heuristic applies **only** when every element on both sides is an object with a unique
scalar id under `id`, `uuid`, `key` or `name`. A partial match would mean two matching
strategies inside one array, producing output nobody can reason about. Two tests pin the
refusal: one array with a missing id, one with duplicates.
When id matching applies, the path names the id (`items[id=b].v`) rather than a position,
because the element may have moved and a positional path would be a lie.

**Headers get their own comparison rather than being coerced into an object.**
The tempting shortcut is to build a `map[string]string` and reuse the JSON walk. It
silently collapses duplicate `Set-Cookie` headers before the comparison begins — undoing
the decision from [07](07-storing-a-request.md) that made them a list in the first place.
Names are grouped case-insensitively, because two captures from one provider can differ in
casing for no meaningful reason, and values for one name are compared as a joined group:
order within a name is meaningful, and a per-element diff of two cookies is noise.

**Volatile headers are filtered by default and the response says so.**
`Date`, signatures and request ids differ on every request and drown the two fields
somebody is looking for. But a diff that silently hides fields is a diff that lies — so
`volatile_hidden` is in the response, the list of what was hidden is in the response, and
`?volatile=1` brings them back.
*Wrong call if* the signature is the thing being investigated, which is exactly why the
toggle exists rather than the filter being unconditional.

**Request-line differences are reported separately from headers and body.**
Method, path and query are neither, and "the same payload went to a different path" is a
routing bug rather than a payload bug — frequently the entire answer, and invisible if the
diff only looks inside the body.

**Non-JSON bodies fall back to a whole-body comparison and say so.**
A wrong answer dressed as a structural diff would be worse than an honest "these are not
JSON". The `Note` field carries that.

**Trailing content makes a body non-JSON.**
`{"a":1} oops` decodes an object and leaves the rest, which would let a malformed body
render as structured data. `dec.More()` catches it.

**`Identical` is a field, not `len(Changes) == 0` at the call site.**
"These are the same" is an answer worth stating, and an empty list at the UI looks like a
failure to compute one.

**The diff endpoint is a `GET`, unlike verify and replay.**
Nothing here is a secret, both ids are authenticated by the same token already, and a diff
is a read worth linking to. Consistency with the neighbouring endpoints is worth less than
a URL somebody can paste into a ticket.

**Both ids are authenticated, separately.**
Checking only the first would turn this endpoint into a way to read **any** capture by
diffing it against one you own. There is a test with two inboxes that asserts 401 in both
directions.

## Walkthrough

### `internal/diff/diff.go`

`decode` (`:80`) — `UseNumber` and the `dec.More()` guard, each with the failure it
prevents.

`walk` (`:100`) dispatches on the left type and falls through to a whole-value change when
the right type differs.

`walkObjects` (`:124`) — the sorted union.

`walkArrays` (`:168`) picks a strategy; `elementIDs` (`:212`) is the heuristic and its
refusal conditions.

`walkArraysByID` (`:191`) names paths by id, not index.

### `internal/diff/headers.go`

`Headers` (`:16`) and the comment on why this is not the JSON walk.

`VolatileHeaders` (`:70`) is exported so the caller can offer it as a toggle — the decision
belongs with the person who knows whether the timestamp is what they are investigating.

### `internal/server/diff.go`

`handleDiff` (`:18`) — the second `AuthenticateRequest` (`:45`) is the access-control line,
with the comment saying what its absence would allow.

`appendIfDifferent` (`:88`) builds the request-line changes.

## Verified

**Twenty-four tests.** The two that justify the package existing come first:
`TestKeyOrderIsNotAChange` reorders every key at two nesting levels and asserts
**identical**, and `TestWhitespaceIsNotAChange` does the same for formatting. A text diff
fails both.

`TestPlanAcceptanceCriterion` is `PLAN.md`'s wording as a test: two payment payloads
differing only in `amount`, asserting **exactly one** change at `data.object.amount` with
`2500 → 9900`.

The array pair is the interesting one. `TestArrayByIndexOnInsertion` *documents the bad
behaviour* — inserting at the front reports every position — and `TestArrayByIDSurvivesInsertion`
shows the same insertion producing exactly one change when the elements carry ids.
`TestArrayByIDMatchesMovedElements` swaps two elements and changes one field, and asserts
one change naming `id=b`.

`TestLargeIntegersKeepPrecision` fails under default JSON decoding.
`TestOutputIsDeterministic` runs fifty times.
`TestHeadersDiff` checks that a name differing only in casing is **not** a change while a
lost duplicate `Set-Cookie` **is**.

Seven endpoint tests, including `TestDiffEndpointAuthenticatesBothIDs` with two separate
inboxes asserting 401 in both directions.
