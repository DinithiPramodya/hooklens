import { useEffect, useState } from 'react'

type Health = { status: string }

/**
 * Phase 2 unit 1 placeholder.
 *
 * This exists to prove the whole pipeline end to end: Vite builds into a Go
 * package, go:embed compiles the output into the binary, and the binary serves
 * it from the same origin as the API -- so this fetch needs no base URL, no
 * CORS configuration, and no knowledge of where the server lives.
 *
 * The real inspector UI arrives over units 13-15.
 */
export default function App() {
  const [health, setHealth] = useState<Health | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    // A relative URL. In production the binary serves both this page and the
    // API; in development Vite proxies /healthz through to :8080. Either way
    // the browser sees one origin.
    fetch('/healthz')
      .then((r) => (r.ok ? r.json() : Promise.reject(new Error(`HTTP ${r.status}`))))
      .then(setHealth)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)))
  }, [])

  return (
    <main>
      <h1>hooklens</h1>
      <p className="tagline">
        see what your webhooks actually send — then send them to localhost
      </p>

      <section>
        <h2>server</h2>
        {health && <p className="ok">reachable — status “{health.status}”</p>}
        {error && <p className="err">unreachable — {error}</p>}
        {!health && !error && <p className="muted">checking…</p>}
      </section>

      <footer>
        Phase 2, unit 1 — the build pipeline works. The inspector lands in units 13–15.
      </footer>
    </main>
  )
}
