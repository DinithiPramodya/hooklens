# 36 — The README as a product page

> Phase 6 · unit 36. The first launch unit, and the only one of them that is
> engineering rather than publishing.

## Brief

**What is this thing?** The README is the landing page. For a self-hosted developer
tool it is not *a* marketing surface, it is the only one — there is no website, no
sales call, no onboarding email. Somebody arrives from a link, spends somewhere
between five and thirty seconds deciding whether this is worth their afternoon, and
either scrolls or leaves. Everything else in the repository is read by people who
already decided.

**What problem does it exist to solve?** The gap between "what this software does"
and "what a stranger can tell it does in fifteen seconds". Those are very different
documents, and the default failure is to write the first and believe you wrote the
second. The classic shape is a README that opens with installation instructions —
perfectly useful, and it answers a question the reader has not yet decided to ask.
Before READMEs, this job belonged to a project's homepage; the convention moved into
the repository because that is where the link now points, and GitHub renders it as
the first screen.

**How does it actually work underneath?** Three mechanisms, none of them mysterious.

*Progressive disclosure.* The reader's questions arrive in a fixed order — **what is
it, why should I care, show me, how do I run it, how does it work, how do I
contribute** — and each one is only asked if the previous was answered. So the
document is ordered by the reader's decision sequence, not by the author's mental
model of the system. A "Configuration" table above a quickstart is answering question
four before question one.

*The demonstration beats the description.* A screenshot or an animated GIF near the
top converts far better than any paragraph, because it is the only element that
proves the thing exists and works. Text describing a UI is a claim; a picture of the
UI is evidence.

*Time-to-first-success.* The single most predictive number for whether somebody
adopts a developer tool is how long from landing to a working thing. Every
prerequisite, every step, every "you'll also need" is a place people leave. That is
why the quickstart is measured in commands and seconds rather than completeness — it
is deliberately not the full install guide, and the full guide lives below it.

**What are the sharp edges?**

- **Writing for yourself.** You know what a webhook inspector is. The reader may
  know, or may have arrived from a search for "test stripe webhook locally".
- **A status section that ages.** "Phase 4 of 6 complete" is honest and useful during
  development and becomes noise the moment it is stale — and it is stale the day
  after every merge.
- **Burying the demo below the fold** behind badges, a table of contents and a
  requirements list.
- **A quickstart that does not work on a clean machine**, because it quietly assumes
  something already present on yours.
- **Overclaiming**, which for a self-hosted tool is uniquely expensive: the reader
  will find out in ten minutes, having installed it.

**In hooklens:** the current README is a good *developer* README and a poor product
page. It opens with three solid sentences, then spends twenty lines on a build-status
paragraph before anyone has seen what the thing does, and puts requirements above the
quickstart. The work here is reordering for the reader's questions, adding an
architecture diagram (the one genuinely load-bearing explanation — a tunnel is not
obvious), writing a self-host section that stands alone, and replacing the phase
narrative with something that does not rot. The animated GIF is a placeholder: it
needs a recording, which is the author's to make.

---

## Decisions

**Ordered by the reader's questions, not by the system's structure.**
The old README went: what it is → twenty-line status paragraph → requirements → run
it → commands → configuration. That is the author's mental model, and it answers
"what version of Node do I need" before the reader has decided they want it. The new
order is what it does → quickstart → how it works → self-hosting → what is still open
→ development. Requirements moved *below* the quickstart, which looks backwards and
is not: somebody skimming needs to see the four commands before they care whether
they have Node.
**Wrong call if** the audience were contributors rather than users — a
`CONTRIBUTING.md` audience wants the development section first. That is an argument
for two documents, and this repository is small enough that one with the right
ordering is better than two that drift.

**The status paragraph goes, replaced by two things that do not rot.**
"Phase 4 of 6 complete" followed by twenty lines of what is built was genuinely
useful during development and was already stale — Phase 5 shipped days ago and it
still said Phase 4. Worse, it occupied the space where the demo belongs.
It is replaced by a short "not yet released" note and a **What is still open**
section listing three specific, checkable things: no licence, no tagged release, and
a throughput target not met. Those are also stale-able, but each names a condition
that makes it obviously false when it changes, rather than a phase number that needs
manual bumping. Being specific about unmet goals is also just better: a reader who
finds the load-test caveat themselves trusts the rest less.

**A placeholder comment for the GIF, not a silent gap.**
The demo is the single highest-value element and I cannot produce it — it needs a
screen recording. Rather than leaving the space empty, there is an HTML comment at
exactly the right position saying what to record, how long, and the one line to
replace it with. An empty space is a thing nobody remembers; a comment in the
rendered-invisible spot is a task in the place it belongs.

**ASCII diagram, not Mermaid.**
GitHub renders Mermaid, which is the obvious choice. Rejected because this README is
also read with `cat` and `less`, in a terminal, and by anyone browsing the repository
outside GitHub — where Mermaid degrades to a block of unreadable source in the middle
of the page. ASCII renders everywhere and degrades to nothing.
**Wrong call if** the diagram gets complex enough that hand-drawn boxes become a
maintenance burden. At seven boxes it is fine.
**The signal to switch** is the first time updating the diagram is why a change did
not get documented.

**The diagram explains one thing, and it is NAT.**
A component diagram of every package would be accurate and would teach nothing — the
reader can see the package list. The only genuinely non-obvious thing about this
architecture is *why the tunnel connection points the direction it does*, so the
diagram is built around that single arrow and says in words that NAT will not let you
reverse it. A diagram that explains one thing well beats a diagram that shows
everything.

**`brew install` shown, and explicitly marked as not working.**
Tempting to omit it until it works, or to show it and let people find out. Neither:
it is shown, immediately followed by "neither works yet — the tap and bucket are not
published", with a link to the config that will produce them. Showing the intended
install path tells the reader what kind of project this is; the disclaimer is
non-negotiable because a `brew install` that fails is a reader who now distrusts
everything else on the page.

**Two properties promoted from the notes into the README.**
Durability-before-response, and never-silently-truncate. These are in
[06](06-reading-a-request.md) and [24](24-size-limits.md) with full reasoning, and a
line each in the README, because they are the two things somebody evaluating whether
to *trust* the tool with their production webhooks actually needs to know — and they
are exactly the properties a competitor's marketing page would not mention.

**Three recommended notes, not thirty-six.**
The full list is now behind a `<details>`. Thirty-six links is a wall that gets
skipped entirely; three with a sentence each about why gets read. The three chosen
are the ones that answer "is this person any good" rather than the ones that are most
useful for building — a different question, and the right one for a page whose reader
has not committed to anything.

---

## Walkthrough

Sections in order, and what each is for.

**The three lines and the bold first line (`README.md:1`).** A bolded sentence that
is a *promise*, not a description: "See exactly what a webhook sent you — then send it
to your laptop." Then three lines naming the capabilities, then "One binary,
self-hosted, no account", which is positioning — it says what this is *instead of*
without naming a competitor.

**The GIF placeholder (`:9`).** Immediately after, before anything else, because
below the fold it does not exist.

**The release caveat (`:18`).** Short, and links forward to the detail rather than
carrying it.

**What it does (`:24`).** Seven bullets, each leading with the capability and then the
reason it is not trivial — "every header with duplicates and ordering intact, and the
body as raw bytes — never parsed and re-serialised, so a signature still verifies".
The second clause is what distinguishes this from a page of feature nouns.

**Quickstart (`:41`).** Four commands, then one `curl`, then the payoff sentence: "It
appears in the list before you switch windows back." The tunnel is shown next with
its real terminal output, because that output *is* the product.

**How it works (`:91`).** The NAT paragraph first, in prose, then the diagram, then
the two trust properties. The prose before the diagram is deliberate: a diagram
answers "what connects to what" and cannot answer "why", and the why is the whole
point here.

**Self-hosting (`:134`).** Written to stand alone — three commands, then the four
things a real deployment needs that localhost does not. `/metrics` not being public
is called out here rather than left in the metrics note, because this is where
somebody about to expose a port is reading.

**Configuration (`:160`)** now includes `HOOKLENS_MAX_BODY`, `HOOKLENS_RATE_CREATE`
and `HOOKLENS_RATE_CAPTURE`, which existed since Phase 5 and were never documented —
found by checking the table against `internal/config/config.go` rather than by
remembering.

**What is still open (`:202`).** Three specific gaps. Its position is the decision:
before Development, so a reader meets it while deciding, not after.

**Development (`:213`) onward** is the old README, mostly unchanged and now correctly
placed last. Requirements, checks, load test, race detector, layout.

**Docs (`:307`).** Three recommendations with reasons, then the full list in a
`<details>`.

### Checked rather than remembered

Everything asserted on the page was verified against the code:

- `HOOKLENS_MAX_BODY` default is `1 << 20` — `internal/capture/request.go:24`.
- Rate defaults 10/minute and 50/second — `internal/config/config.go:78`.
- `HOOKLENS_SERVER` exists and defaults to `http://localhost:8080` —
  `cmd/hooklens/forward.go:147`.
- The landing page really does mint an inbox on click — `web/src/App.tsx:27` calls
  `createInbox()`, so "click to create an inbox" is accurate.
- The layout block was rewritten from `ls internal/`; the first draft omitted
  `capture`, `secret` and `sweeper`, which is exactly the failure mode of writing a
  file listing from memory.

---

## Verified

- `gofmt -l .`, `go vet ./...` clean; no code changed in this unit.
- Every internal link target exists (`docs/learn/05`, `17`, `19`, `24`, `33`, `35`,
  `36`, `PLAN.md`, `CLAUDE.md`, `.goreleaser.yaml`, `docs/QUIZ.md`,
  `docs/learn/README.md`).
- Every configuration default and CLI flag on the page checked against the source,
  as listed above.
- **Not verified:** how it renders on GitHub, since nothing is pushed. The ASCII
  diagram is inside a fenced block so it will not be reflowed, and `<details>` is
  supported — but "it looks right in a terminal" is not the same claim as "it looks
  right on GitHub", and only one of those has been tested.
- **Still missing and not producible here:** the demo GIF. It is the most valuable
  thing on the page and it needs a screen recording.

---

## Postscript — recording the demo

`docs/demo.gif` replaced the placeholder: a new inbox, a Stripe-style and a
GitHub-style webhook arriving live without a refresh, and the Stripe payload expanded
as a JSON tree. Six frames, 10.3 seconds a loop, 480 KB. Recorded in the browser
only, so the `hooklens forward` terminal half of the story is not in it; the README's
quickstart shows that output as text directly below.

Two things the recording found, which is the argument for making one at all:

- **The capture list rendered paths one letter per line.** The stacked row layout
  keyed on the *viewport* width, but the list lives in a half-width pane, so on a
  wide screen the list itself was narrow and never stacked. The path's `1fr` track
  then shrank to its min-content — which `overflow-wrap: anywhere` makes a single
  character. Fixed with a container query on the list's own width
  (`web/src/index.css`). No test measures layout; this was found by looking.
- **The footer still said "Phase 3".** A status line in the UI rots exactly like
  the one this unit removed from the README. It now tells users that shift-click
  compares two captures, which nothing else on the page mentioned.

### The re-take

The first GIF was replaced, because checking it frame by frame showed it was weaker
than it looked: 1,536 px wide with the content in the middle 40% (so GitHub shrank
the text to about 10 px), a red "not forwarded" box as the first thing in the detail
pane, and a final click that missed after the page scrolled.

The re-take ran with the tunnel attached to a local app, in a 767 px window, and it
found two more things before it was right:

- **The live delivery bug** recorded in [25](25-showing-delivery.md): a delivered
  capture showed as not attempted until reload.
- **The row list jumped on selection.** A hint line ("shift-click another capture…")
  was rendered only once a row was selected, pushing every row down ~38 px under the
  cursor — so a quick second click landed on the wrong capture, and the recorder drew
  its click marker on the row that was *not* selected. The line is now always
  reserved when there are two captures, and only its text is hidden
  (`web/src/App.tsx`).

And one lesson about the tool rather than the product: **the browser recorder saves a
frame per action, not per screenshot.** The second attempt silently omitted the three
frames that mattered most — webhooks arriving — because they happened between
actions. It was only caught by decoding the GIF and reading every frame, not by
looking at the one frame an image viewer shows. The final recording hovers after each
webhook to force a frame, and exports without click markers.

Final: 15 frames, 767×639, about 17 seconds a loop, 1.2 MB.
