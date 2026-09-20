// Package webui embeds the built frontend into the binary and serves it.
//
// The dist directory is produced by `npm run build` in web/, which is
// configured to write here rather than to web/dist -- //go:embed cannot reach
// outside its own package directory, so the embed declaration has to sit beside
// the files. See docs/learn/11-spa-and-go-embed.md.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// dist holds the built frontend.
//
// The "all:" prefix includes files beginning with a dot, which plain //go:embed
// skips. That matters for exactly one file: dist/.gitkeep, which is committed so
// that a fresh clone with no npm build still COMPILES. Without it the build
// fails with "pattern dist: no matching files found" before you get any hint of
// what is actually wrong.
//
// Compiling is not the same as working, though -- see Available.
//
//go:embed all:dist
var dist embed.FS

// FS returns the built frontend rooted at dist/, so paths are "index.html"
// rather than "dist/index.html".
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// Only reachable if the embed directive above stops matching, which is
		// a compile-time-shaped problem surfacing at runtime. Nothing sensible
		// to recover to.
		panic("webui: dist not embedded: " + err.Error())
	}
	return sub
}

// Available reports whether a real frontend build is embedded.
//
// A clone that has never run `npm run build` compiles fine (thanks to .gitkeep)
// and then serves nothing, silently. This is what turns that into a diagnosable
// message instead of a blank page.
func Available() bool {
	_, err := fs.Stat(FS(), "index.html")
	return err == nil
}

// Handler serves the embedded SPA.
//
// Two behaviours matter and neither is the default you get from
// http.FileServer.
func Handler() http.Handler {
	files := FS()
	server := http.FileServer(http.FS(files))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !Available() {
			notBuilt(w)
			return
		}

		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}

		// Does this path name a real file?
		if _, err := fs.Stat(files, p); err != nil {
			// No -- so it is a client-side route like /requests/abc123. Serve
			// the shell and let the JavaScript router work out what to render.
			// This is the "SPA fallback", and it is the only reason deep links
			// work at all: the server has never heard of that path.
			//
			// Note this handler is only ever reached for paths the API mux did
			// not claim. If the fallback were mounted ahead of the API, a typo'd
			// endpoint would return 200 and an HTML document, and the client
			// would try to JSON.parse a web page.
			serveIndex(w, files)
			return
		}

		// A real file. Cache headers go in OPPOSITE directions depending on
		// which file it is, and getting them backwards is a classic.
		if strings.HasPrefix(p, "assets/") {
			// Vite gives these content-hashed names (index-Cb3gDD6b.js), so the
			// name changes whenever the content does. They can therefore be
			// cached permanently -- immutable tells the browser not to even
			// revalidate.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			// index.html and anything else unhashed. Never cache: index.html is
			// what NAMES the hashed bundles, so a stale copy pins a user to an
			// old application version with no way to escape it.
			w.Header().Set("Cache-Control", "no-cache")
		}

		server.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, files fs.FS) {
	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		notBuilt(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	// 200, not 404. As far as the browser is concerned this IS the page; the
	// router decides afterwards whether the route exists.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(index)
}

func notBuilt(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`<!doctype html>
<meta charset="utf-8">
<title>hooklens — frontend not built</title>
<body style="font-family:system-ui;max-width:40rem;margin:4rem auto;padding:0 1.5rem;line-height:1.6">
<h1>Frontend not built</h1>
<p>This binary was compiled without a frontend build embedded. The API is running
normally; only the UI is missing.</p>
<pre style="background:#eee;padding:1rem;border-radius:6px">cd web
npm install
npm run build</pre>
<p>Then rebuild the binary. See <code>docs/learn/11-spa-and-go-embed.md</code>.</p>
</body>`))
}
