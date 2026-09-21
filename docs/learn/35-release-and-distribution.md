# 35 — Cross-compilation, static binaries, and how a `brew install` works

> Phase 5 · unit 35. The last build unit before launch: turning one repository into
> something a stranger installs with one line on three operating systems.

## Brief

**What is this thing?** Release engineering: producing, for every platform you
support, an artifact a user can run, plus the metadata that lets a package manager
find it, verify it and install it. For a Go project that is a set of compiled
binaries, checksums, and two or three small files in other repositories that tell
Homebrew and Scoop where those binaries are.

**What problem does it exist to solve?** "Clone the repo and run `go build`" is a
fine instruction for people who already have Go installed and are already convinced.
Everybody else leaves. Before packaging, distributing software meant either shipping
source and hoping, or maintaining a build machine per platform — the reason
"works on my machine" became a joke is that for compiled software it was literally
the state of the art. The specific thing this solves is that *the person installing
has none of your tools*, and the distance between "I read about this" and "it is
running" is where almost all of your potential users are lost.

**How does it actually work underneath?** Three mechanisms stacked.

*Cross-compilation.* A compiler turns source into machine code for a particular
instruction set and a particular operating system's syscall and executable
conventions. Normally the compiler emits code for the machine it runs on. Go's
toolchain instead carries the code generators and the standard library for every
platform it supports, so `GOOS=darwin GOARCH=arm64 go build` on a Windows machine
produces a Mac binary — no VM, no toolchain per target. That works because the
standard library is written in Go and gets recompiled for the target too.

*Static linking.* A dynamically linked binary is incomplete: it contains references
to shared libraries the loader must find at startup, and if the target machine has
a different libc it does not run. A static binary contains everything. Pure Go
programs are static by default — but the moment cgo is involved (which the `net` and
`os/user` packages historically pull in for DNS and user lookup), the binary links
against the host's libc and stops being portable. `CGO_ENABLED=0` forces the pure-Go
implementations and is the difference between a binary that runs on any Linux and
one that runs on the Linux it was built on.

*Package manager taps and buckets.* Homebrew's `brew install user/tap/tool` resolves
to a GitHub repository named `homebrew-tap` containing a Ruby "formula": a URL, a
SHA-256, and instructions. Scoop's buckets are the same idea in JSON. Neither hosts
your binary — they are indexes pointing at your release assets, which is why the
checksum matters: it is the only thing standing between the index and a substituted
download.

**What are the sharp edges?**

- **`CGO_ENABLED` defaults to 1 when a C compiler is present and 0 when it is not**,
  so the same command produces a different kind of binary on different machines, and
  the failure appears on a user's machine rather than yours.
- **Version information has to be injected at build time** with `-ldflags -X`,
  because a binary cannot read the git tag it was built from.
- **Checksums are the security boundary**, and unsigned releases mean a compromised
  release page is a compromised install.
- **macOS Gatekeeper** refuses to run unsigned, unnotarized binaries downloaded from
  the internet in a way that looks, to the user, like the program is broken.
- **A stale formula is worse than no formula**: it installs an old version silently.

**In hooklens:** one binary serving API, UI ([11](11-spa-and-go-embed.md)) and the
tunnel CLI ([21](21-the-cli.md)), which makes the packaging question unusually
simple — there is exactly one artifact. The work is a GoReleaser config that builds
it for macOS, Linux and Windows on amd64 and arm64, injects the version, emits
checksums, and publishes the formula and manifest. The part I can verify here, with
no GitHub access, is everything up to the publish: that the config is right, that
every target actually cross-compiles, and that the embedded UI survives the trip.

---

## Decisions

**GoReleaser, not a Makefile with a build matrix.**
The alternative is genuinely viable: `for` over `GOOS`/`GOARCH`, `tar`, `sha256sum`,
`gh release create`. Maybe forty lines. What that does not give you is the second
half — generating a Homebrew formula with the right SHA-256 per platform, committing
it to a different repository, and doing the same for Scoop, on every release,
without drift. That is the part that rots when done by hand, and a stale formula is
worse than no formula because it installs an old version silently.
**This is the wrong call if** the project ever needs something GoReleaser does not
model, at which point fighting its config is worse than the forty lines.
**The signal to switch** is the first release step expressed as a shell hook to work
around the tool rather than with it.

**`CGO_ENABLED=0`, set explicitly rather than relied on.**
Go picks cgo on or off depending on whether a C compiler is present on the *build*
machine, so the same command produces a differently-linked binary on a developer's
laptop and on a CI runner. With cgo, `net` and `os/user` link against the host libc
and the binary then fails on any system with an older one — on the user's machine,
not on ours, which is the worst place for a build decision to surface. Setting it in
the config makes the linking mode a property of the release rather than of whoever
ran it.
**Wrong call if** something here ever needs cgo — SQLite, a C crypto library. Then
the static-binary promise goes away and the packaging story needs rethinking, not
patching.

**Five targets, not six: no `windows/arm64`.**
Every extra target is an artifact nobody tests, a checksum line to explain, and
another thing that can fail a release at 11pm. Windows on ARM is real and the
audience for this CLI there is currently nobody. Recorded as a decision rather than
an oversight so that adding it later is a one-line change with a reason attached.

**`-trimpath` and `mod_timestamp`, for reproducibility.**
Without them, two builds of the same commit differ: one embeds
`C:\Users\Dinithi\...` in every path, and both embed their own build time.
Reproducibility is not a nicety — it is the only way anyone can check that the
published binary corresponds to the published source, which is the entire value of
a checksum in a formula.

**`-s -w`: strip symbols, accept the cost.**
About 30% off the binary. The cost is real: a stack trace from a user's crash has no
symbol names. Acceptable here because panics are logged with our own context
(`withRecover`, unit 6) rather than diagnosed from a core dump, and because a 12MB
download versus 17MB is the difference a first-time user actually notices.

**Version injected with `-ldflags -X`, and a `version` subcommand to read it.**
A binary cannot read the git tag it was built from. `main.version` defaults to
`"dev"`, which is the correct answer for a `go build` from a working tree. The
subcommand had to be dispatched **above** `config.Load()`, like `forward`: the
Homebrew formula's test block is literally `hooklens version`, so a version command
that needs a `DATABASE_URL` is a failing `brew install`. That is the kind of thing
you find by writing the formula, not by writing the command.

**No `license:` field, and that blocks the first release.**
The repository declares no licence anywhere, and choosing one is the author's
decision, not something a build tool should assert on their behalf — so the field is
commented out with the reason rather than filled in with a plausible guess. Worth
being blunt about the consequence: "no licence" legally means *all rights reserved*,
so anybody who runs `brew install` has no permission to use what they installed.
This has to be settled before Phase 6, and it is the one item in this unit that
cannot be decided here.

**Tags trigger releases, not pushes to main.**
`on: push: tags: ["v*"]`. Publishing a release per merge is how a version number
stops meaning anything. A release is an explicit act.

**Two separate PATs for the tap and the bucket.**
The default `GITHUB_TOKEN` is scoped to the repository the workflow runs in and
**silently cannot write to another one** — which surfaces as a release that
publishes perfectly and a formula that never updates. Two tokens rather than one so
either can be revoked without breaking the other.

**`fetch-depth: 0` on the checkout.**
GoReleaser builds the changelog from the commits between tags, and a shallow clone
has no history to read. The release notes come out empty, and nobody notices until
after publishing.

---

## Walkthrough

### `.goreleaser.yaml`

**`before.hooks` (`:11`).** `npm ci && npm run build` in `web/`, before any Go build.
Not optional and not merely an optimisation: `//go:embed all:dist` reads
`internal/webui/dist` at compile time, and `dist/.gitkeep` is enough to satisfy the
directive. So without this hook the build **succeeds** and produces five binaries
that serve no UI. A silent wrong answer, shipped to users — the same ordering hazard
already documented in `.github/workflows/ci.yml`.

**`builds.env` (`:30`).** One line, `CGO_ENABLED=0`, with the longest comment in the
file, because it is the line whose absence fails on somebody else's computer.

**`builds.ldflags` (`:58`).** `-s -w` then `-X main.version={{.Version}}`. The `{{ }}`
is GoReleaser's template context, filled from the git tag.

**`archives` (`:73`).** `tar.gz` everywhere, `.zip` on Windows — a `.tar.gz` on
Windows is a support question, not a file. The `name_template` puts version, OS and
arch in the filename, which is what the formula's URL template interpolates against.

**`checksums` (`:87`).** The security boundary of the entire distribution story.
Homebrew and Scoop do not host the binary; they point at a URL and verify a hash. If
the hash is wrong or absent, the index is only as trustworthy as the release page.

**`changelog` (`:99`).** Grouped by conventional-commit prefix, with `docs:`, `test:`,
`chore:` and `ci:` excluded. On this project that exclusion matters more than usual —
nearly every commit touches `docs/learn/`, and including them would bury the three
lines a reader actually needs.

**`brews` (`:138`).** The block that makes `brew install DinithiPramodya/tap/hooklens`
work. A "tap" is nothing more than a GitHub repository named `homebrew-tap`
containing Ruby formulas; the short form resolves to it by convention. GoReleaser
generates `Formula/hooklens.rb` with the per-platform URLs and SHA-256s and commits
it there on every release. The `token` is a PAT for the reason in the decisions
above.

**`scoops` (`:166`).** The same model for Windows, in JSON.

### `.github/workflows/release.yml`

Tag-triggered, `contents: write` (the default read-only token cannot create a
release), `fetch-depth: 0` for the changelog, Node before Go for the embed, and the
three tokens. Twenty lines of YAML and four of them are load-bearing in a way that
fails quietly if wrong, which is why each carries its reason.

### `cmd/hooklens/main.go:49`

The `version` subcommand, dispatched above `config.Load()` alongside `forward`, and
accepting `version`, `--version` and `-v` because all three are what people type.

---

## Verified

- `go build ./...`, `go vet ./...`, `gofmt -l .` clean; `go test ./... -count=1`
  green.
- `go run ./cmd/hooklens version` prints `hooklens dev` — correct for a build from a
  working tree with no tag.
- **Cross-compilation, partially verified.** With `CGO_ENABLED=0 -trimpath -ldflags
  "-s -w -X main.version=..."`:

  | target | result | size |
  |---|---|---|
  | linux/amd64 | built | 12M |
  | linux/arm64 | built | 12M |
  | darwin/amd64 | built | 13M |
  | darwin/arm64 | **not verified** | — |
  | windows/amd64 | **not verified** | — |

  The run was stopped part-way by the machine running out of memory — the
  `darwin/arm64` line failed with a bare `compile.exe: exit status 1` from
  `internal/poll` and no diagnostic, which is the signature of the compiler being
  killed rather than of a compile error, and `windows/amd64` never ran. I am
  recording this as unverified rather than inferring success from the other three:
  `darwin/arm64` shares its `GOOS` with a target that built and its `GOARCH` with
  another, which is suggestive and is not evidence.

  To finish the check when the machine has room:

  ```sh
  for t in darwin/arm64 windows/amd64; do
    CGO_ENABLED=0 GOOS=${t%/*} GOARCH=${t#*/} go build -trimpath \
      -ldflags "-s -w -X main.version=v0.0.0-test" -o /tmp/hooklens ./cmd/hooklens
  done
  ```

- **Not verifiable here, and stated rather than glossed:** `goreleaser` is not
  installed on this machine, so the config has not been run through
  `goreleaser check` or `goreleaser build --snapshot`. The YAML is hand-written
  against the v2 schema. The first tagged release will be the first execution of
  this file, which is the normal situation for release automation and is still worth
  saying out loud. Neither the Homebrew tap nor the Scoop bucket repository exists
  yet, and neither PAT has been created — both are GitHub-side setup, and this
  session is not pushing anything.
