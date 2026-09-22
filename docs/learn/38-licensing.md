# 38 — What a licence actually does

> Phase 6 · unit 38. One of the two things blocking the first public release. Not a
> build step: a grant of permission, written once, that every future user relies on.

## Brief

**What is this thing?** A licence is a standing grant of permission from the
copyright holder to everyone else, published alongside the work, saying what they
may do with it and on what conditions. It is not a sale and not a transfer — the
author keeps the copyright. It is closer to a notice nailed to a gate: *you may
come through, if you do these things.*

**What problem does it exist to solve?** Go down one layer, to copyright itself.
Copyright attaches automatically, on creation, with no registration and no notice
required — the moment a file is saved it exists. And it is **default-deny**: the
holder gets the exclusive right to copy, modify and distribute, and by definition
nobody else has any of those rights. "Exclusive" is the whole mechanism. So the
natural state of an unlicensed repository is not *free for all*, it is *nobody may
touch this*. Publishing to GitHub does not change that; it grants only what
GitHub's terms grant, which is viewing and forking within GitHub itself. A stranger
who downloads a release binary and runs it at work is, strictly, infringing.

Before standard licences, permission was negotiated per user — a contract, a
lawyer, a signature. That does not scale to a thousand anonymous downloads. The
standard licences turned permission into a public, pre-negotiated constant: read
the file, you already have your answer, nobody is contacted.

**How does it actually work underneath?** It is a unilateral offer, accepted by
conduct — you use the software, you have accepted. Three parts do the work:

*The grant.* The verbs you are allowed: use, copy, modify, merge, publish,
distribute, sublicense, sell.

*The conditions.* What you must do in exchange — nearly always "keep this notice
attached", sometimes much more. This is where the teeth are. Permission is
**conditional**, so breaching a condition does not make you a bad contract partner,
it makes the permission evaporate, which drops you back to default-deny. That is
why copyleft is enforceable at all: the remedy is copyright infringement, not a
contract dispute.

*The disclaimer.* The shouty capitals nobody reads — no warranty, no liability.
Practically the most valuable clause in the file for the author. Without it, the
legal position of "I gave away a tool that mangled your production data" is a lot
less comfortable.

On top sits an SPDX identifier (`MIT`, `Apache-2.0`, `AGPL-3.0`), a short machine-
readable name. That is what GitHub's sidebar badge, Homebrew manifests, and the
corporate licence scanners that gate adoption actually consume — not the prose.

**What are the sharp edges?**

- **No licence is the most restrictive state, not the most permissive.** It reads
  as generous and behaves as "all rights reserved".
- **You cannot un-grant.** A version released under MIT stays MIT forever. Future
  versions can change; shipped ones cannot be recalled.
- **Relicensing later needs every copyright holder's agreement.** Today that is one
  person, so it is free. Each outside contributor raises the price permanently.
- **Dependency terms flow upward through distribution.** A compiled binary is a
  combined work. MIT, BSD and Apache dependencies impose nothing awkward; a single
  GPL dependency would impose conditions on the whole distributed artifact.
- **Copyleft has an adoption cost.** Many companies ban AGPL by blanket policy, so
  the protection is real but so is the deterrence.
- **Patents are separate from copyright.** MIT is silent on them; Apache-2.0 grants
  them explicitly and terminates the grant if you sue over patents.
- **A licence is not a trademark.** It governs the code, never the name.

**Here is how this shows up in hooklens.** Three places already point at this
decision and wait on it. `README.md` lists "No licence" first under *What is still
open*. `docs/learn/README.md` names it as one of two blockers on the first public
release. And `.goreleaser.yaml` has the `license:` field commented out in both the
Homebrew and Scoop blocks, with a note that the manifests must carry it before a
tag — because those manifests are exactly what a corporate scanner reads before
approving an install.

It matters more here than for a library, because of what we ship: one static binary
that embeds the React UI and every Go dependency. A release is the distribution of a
combined work, which is precisely the act a licence governs. And the audience is
developers evaluating a tool in about thirty seconds — an unlicensed repository
reads to that audience as unfinished, whatever the code looks like.

---

## Decisions

**MIT, chosen by the author.** Three real candidates were on the table.

- **Apache-2.0** — permissive like MIT, plus an explicit patent grant that
  terminates if the licensee sues over patents, and a NOTICE-file convention. The
  patent clause is genuinely valuable for large projects with corporate
  contributors. Rejected as more ceremony than a one-author tool needs.
- **AGPL-3.0** — copyleft that reaches network use, so anyone hosting a modified
  hooklens as a service must publish their changes. It is the licence that would
  stop someone reselling this as closed SaaS. Rejected because many companies ban
  AGPL by blanket policy, and the audience for a portfolio project includes exactly
  the engineers at those companies.
- **MIT** — chosen. Short, universally understood, and the default a reader expects
  from a developer tool; nobody evaluating it will pause over the licence.

**This is the wrong call if** someone starts selling a hosted hooklens and that
matters to the author. **The signal to switch** is that moment — and since
relicensing needs every copyright holder's agreement, it is cheapest *now*, while
there is exactly one. Shipped MIT versions stay MIT forever either way.

**Copyright holder is `DinithiPramodya`**, the name every commit in the repository
is authored under. A licence's copyright line names who is granting permission;
using the same identity as the git history keeps the two consistent. The author can
replace it with a legal name at any time — it is their line, not a build detail.

**LICENSE goes inside every release archive, not only in the repository.** MIT's
one real condition is that the notice accompanies copies of the software, and a
downloaded archive *is* a copy. Before this change `.goreleaser.yaml` packaged the
README and the learn index but not the licence, so the first release would have
shipped binaries in breach of their own licence.

**The SPDX identifier is stated in exactly two places**, `LICENSE` and the
`license: MIT` fields in the Homebrew and Scoop blocks, because those are what
package managers and licence scanners read. The README says it in words and links
to the file, but does not restate the terms — a paraphrase is a third copy that can
drift from the real text.

---

## Walkthrough

- **`LICENSE`** — the standard MIT text, unmodified. It is deliberately not
  paraphrased or trimmed: tools and lawyers both match it verbatim, and an edited
  MIT licence is no longer MIT, it is a custom licence that happens to look like it.
  The only line that varies is the copyright line.
- **`.goreleaser.yaml`, `archives.files`** — `LICENSE` added first, with a comment
  explaining that the archive is a copy and the condition applies to it.
- **`.goreleaser.yaml`, `brews` and `scoops`** — the commented-out placeholder and
  its "this blocks the first release" warning replaced with `license: MIT`.
- **`README.md`** — the "no licence" caveat removed from the release banner and
  from *What is still open*, and a two-line *Licence* section added at the end,
  which is where GitHub readers look for it.

---

## Verified

- `LICENSE` is the canonical MIT text — checked line-for-line against the SPDX
  reference; the only difference is the copyright line.
- `grep -ri licen` across the repository finds no remaining claim that the project is
  unlicensed, outside notes that record the earlier state as history (35, 36).
- GitHub's licence detection reads the root `LICENSE` file; once pushed, the
  repository sidebar should show "MIT License". That is the external check that the
  file is in the form tools recognise.
- **Not verified:** the Homebrew and Scoop manifests' rendering of `license: MIT`,
  since GoReleaser has still never been executed — same caveat as
  [35](35-release-and-distribution.md).
