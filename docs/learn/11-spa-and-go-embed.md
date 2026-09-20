# 11 — SPA vs server-rendered, and what `go:embed` does to the binary

*Phase 2, unit 1. Covers the curriculum bullets "SPA vs server-rendered, and why nothing
here needs SSR" and "what `go:embed` does to the binary".*

## Brief

**What is it?** Two ways of getting HTML into a browser.

**Server-rendered:** for each URL the server assembles complete HTML and sends it. The
browser displays it. Navigate somewhere, and that is a new request producing a new full
page.

**Single-page application:** the server sends one near-empty HTML shell plus a JavaScript
bundle. The JavaScript builds the DOM in the browser and from then on handles navigation
itself, fetching data as JSON rather than pages.

**What problem does it solve?** Everything was server-rendered originally — that *was* the
web. The cost is that every interaction is a full page reload: click a filter and the entire
page is rebuilt, re-sent and re-parsed. You lose scroll position, focus, open menus, and any
state the page was holding. For documents that is fine. For something you *operate* it is
miserable.

SPAs moved rendering into the browser so a click can change one region and leave everything
else alone. The costs are real and worth naming: a bundle must download and parse before
anything appears, crawlers see an empty page, and routing now exists in two places that can
disagree.

Server rendering then came back — Next.js and friends — because those costs bite hardest
exactly where SPAs are least suited: public, content-heavy, SEO-dependent pages.

**How does it work underneath?** The server contract for an SPA is almost absurdly simple:
*for any path that is not a real file, return `index.html`.* That is the **SPA fallback**.
The browser loads the shell, the JavaScript boots, reads `window.location`, and renders
whichever view matches. It is the whole reason a deep link like `/requests/abc123` works —
the server has never heard of that path, it just hands over the shell and lets the client
work it out.

**`go:embed`** is a compile-time directive. `//go:embed dist` makes the Go *compiler* read
those files off disk during the build and write them into the binary's data section,
exposed as an `embed.FS`. At runtime there is no filesystem access at all — the bytes are
already resident, part of the executable.

**Sharp edges.**

- **The files must exist when you compile.** A fresh clone without a frontend build will not
  even compile. Annoying, and honest. The usual bad workaround is committing the build
  output.
- **`go:embed` cannot reach outside its own package directory** — no `../`. The same
  constraint hit with migrations in [03](03-migrations.md).
- **The SPA fallback will happily swallow your API.** If everything unmatched returns
  `index.html`, a typo'd API path returns `200` and an HTML document instead of `404` and
  JSON — and the fetch code then tries to `JSON.parse` a web page. Route order is
  load-bearing.
- **Cache headers go in opposite directions.** `index.html` must never be cached, because it
  names the content-hashed bundles. Those bundles should be cached forever, because their
  names change when they do. Get it backwards and users are either stuck on a stale app or
  re-downloading everything on every visit.

**In hooklens.** Nothing here needs server rendering. It is a tool you operate behind a
capability URL — nothing to index, no SEO, no marketing first paint. And embedding gives the
single binary this project has been building toward: one file to download and run, no second
deployment, and — as [12](12-cors.md) will cover — no CORS to configure, because there is
only ever one origin.

## Decisions

**Vite + React, not Next.js.**
Restating the call from PLAN.md now that it is real. Nothing here needs server rendering:
it is a tool operated behind a capability URL, with nothing to index and no marketing first
paint. Next.js would add a second runtime and a second deployment to solve a problem we do
not have.
*Wrong call if* hooklens ever grows a public marketing surface or docs that need SEO — and
the honest note is that Next.js is the more resume-legible name. That is covered by the
Field Ops Copilot project; picking the wrong architecture here to collect a keyword would
be visible to anyone who asked why.

**Vite builds into `internal/webui/dist`, not `web/dist`.**
`//go:embed` cannot reach outside its own package directory, so the embed declaration has to
sit beside the files. The alternative was a Go package inside `web/` — which would put
`node_modules` on the path `go build ./...` walks. Configuring `build.outDir` costs one line.

**`dist/.gitkeep` is committed, and it is load-bearing.**
The build output is gitignored: it is derived, it churns on every build, and a stale
committed copy is worse than an absent one. But `//go:embed all:dist` **fails to compile**
if the directory has no files at all, so a fresh clone would not build until someone ran
npm — with an error message about embed patterns that says nothing about the real problem.
The `all:` prefix is what includes a dotfile; plain `//go:embed` skips them.
The cost is that compiling no longer implies working, which is why `Available()` exists.

**`Available()` and a "frontend not built" page, rather than a blank screen.**
With `.gitkeep` satisfying the compiler, a clone that never ran `npm run build` produces a
binary that starts cleanly and serves nothing. That is a worse failure than not compiling.
The handler detects the missing `index.html` and returns 503 with the three commands that
fix it. *This is the trade for the decision above, made visible instead of absorbed.*

**An explicit `/api/` catch-all returning JSON 404.**
Not optional. Without it, `GET /api/typo` falls through to the SPA handler, which answers
200 with an HTML document — and the client's `fetch` then tries to `JSON.parse` a web page.
The error surfaces as a syntax error about `<`, nowhere near the actual mistake. Verified:
`/api/typo` → `404 application/json`, `/deep/link/route` → `200 text/html`.

**Cache headers set in opposite directions by path.**
`assets/*` get `max-age=31536000, immutable` because Vite content-hashes their names, so the
name changes whenever the bytes do. Everything else, `index.html` above all, gets
`no-cache` — it is the file that *names* the hashed bundles, so a cached copy pins a user to
an old application version with no way out. Getting these backwards is a classic, and the
two failure modes are "users stuck on a stale app" and "users re-download 220 KB every
visit".

**The SPA fallback returns 200, not 404.**
`/requests/abc123` is a real route — the *client* router knows it even though the server
never will. Returning 404 with the shell attached would make every deep link an error in
the network tab and confuse anything that checks status before parsing.

**`npm ci` in the Dockerfile, not `npm install`.**
`ci` installs exactly the lockfile and fails if `package.json` and the lock disagree;
`install` resolves something newer and rewrites the lock, which makes the image
non-reproducible — the thing the Dockerfile exists to prevent.

**Frontend build step goes before every Go step in CI.**
`//go:embed` reads at compile time, so `go build`, `go vet` and `go test` all need the dist
to exist. Put it after and CI passes while producing a binary with no UI — green, and
wrong. The `lint` job deliberately has no frontend step: `.gitkeep` lets it compile, and
linting Go does not need the assets.

## Walkthrough

### `web/vite.config.ts`

`build.outDir: '../internal/webui/dist'` is the line that makes `go:embed` possible at all.

`server.proxy` matters more than it looks. In development the page is served by Vite on
`:5173` and the API by Go on `:8080` — **two origins**, and every `fetch` would be a CORS
request. The proxy makes the browser see one origin in development too, so the same
relative `fetch('/healthz')` works in both environments with no base URL and no
conditional. That is unit 12's subject, arranged away before it can bite.

### `internal/webui/webui.go`

`//go:embed all:dist` (`:26`) — the `all:` prefix and the committed `.gitkeep` are a pair;
remove either and a fresh clone stops compiling.

`FS()` (`:31`) uses `fs.Sub` to re-root at `dist/`, so handler paths read `index.html`
rather than `dist/index.html`. The `panic` is unreachable unless the embed directive stops
matching, which is a compile-shaped problem surfacing at runtime with nothing to recover to.

`Handler()` (`:56`) does three things `http.FileServer` will not do on its own:

1. Refuses to serve when no build is embedded (`:61`), with a diagnosable page.
2. Falls back to `index.html` for any path that is not a real file (`:72`). The `fs.Stat`
   is the test — "is this a file, or a client-side route?"
3. Sets cache headers per path class (`:88`) before delegating.

The `strings.TrimPrefix(r.URL.Path, "/")` at `:66` is required, not cosmetic: `fs.FS` paths
are unrooted, so `/index.html` does not exist in an `embed.FS` and `index.html` does.

### `internal/server/server.go:92` — `appRoutes`

The doc comment states the ordering explicitly because **registration order is not what
decides it** — Go's `ServeMux` picks the most *specific* matching pattern regardless of
which was registered first. `/api/` beats `/`, and `GET /api/requests/{id}` beats both.
Someone reorganising this file cannot break it by moving lines, but they can break it by
deleting the `/api/` catch-all.

### `Dockerfile`

A third stage, `web`, running `node:24-alpine`. It copies `package.json` and the lockfile
before the source for the same layer-caching reason as `go.mod` — `npm ci` re-runs only when
the lockfile changes.

`COPY --from=web /internal/webui/dist ./internal/webui/dist` (`:38`) overwrites whatever the
build context carried, which is normally just `.gitkeep`, since the directory is gitignored.
Without that line the image compiles and serves the "not built" page.

The runtime stage is unchanged and needs nothing new: the frontend is *inside the binary*,
so there is no static directory to mount and no web server to install.

## Verified

Built locally and in the image, then exercised over HTTP.

| | |
|---|---|
| `/` | 200, the real shell — `<title>hooklens</title>` and hashed asset links |
| `/requests/abc123` | **200 `text/html`** — SPA fallback for a route the server never knew |
| `/api/typo` | **404 `application/json`** — the fallback does *not* swallow the API |
| `/healthz` | 200 JSON — specificity beats the catch-all |
| `index.html` | `Cache-Control: no-cache` |
| `/assets/index-Cb3gDD6b.js` | `Cache-Control: public, max-age=31536000, immutable` |

Bundle 220 KB raw, **69 KB gzipped**. Final image **21.8 MB**, up from 14.7 MB — the whole
frontend costs ~7 MB inside the binary, and nothing outside it.

The container was run against the compose Postgres and served the UI, the API and the
capture endpoint from one process on one port. 20 Go tests still green.
