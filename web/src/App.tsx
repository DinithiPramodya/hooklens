import { useState } from 'react'
import { createInbox } from './lib/api'
import { useLiveCaptures, useRequests, useStoredInbox } from './lib/useInbox'

export default function App() {
  const { inbox, setInbox } = useStoredInbox()
  const { status, dropped } = useLiveCaptures(inbox)
  const { data, isLoading, error } = useRequests(inbox)
  const [creating, setCreating] = useState(false)
  const [createError, setCreateError] = useState<string | null>(null)

  async function onCreate() {
    setCreating(true)
    setCreateError(null)
    try {
      const created = await createInbox()
      setInbox({ slug: created.slug, token: created.token })
    } catch (e) {
      setCreateError(e instanceof Error ? e.message : String(e))
    } finally {
      setCreating(false)
    }
  }

  if (!inbox) {
    return (
      <main>
        <Header />
        <section>
          <h2>get started</h2>
          <p className="muted">
            An inbox gives you a URL to paste into a provider, and a token that is shown
            once and stored only as a hash.
          </p>
          <button onClick={onCreate} disabled={creating}>
            {creating ? 'creating…' : 'Create an inbox'}
          </button>
          {createError && <p className="err">{createError}</p>}
        </section>
      </main>
    )
  }

  const requests = data?.requests ?? []

  return (
    <main>
      <Header />

      <section>
        <div className="row">
          <code className="url">/e/{inbox.slug}/</code>
          <span className={`dot dot-${status}`} aria-hidden="true" />
          <span className="muted">{status}</span>
          <button className="link" onClick={() => setInbox(null)}>
            forget
          </button>
        </div>
      </section>

      <section>
        <h2>
          captures{requests.length > 0 && ` (${requests.length})`}
        </h2>

        {dropped > 0 && (
          <p className="err">missed {dropped} while this tab was behind — reload to resync</p>
        )}
        {isLoading && <p className="muted">loading…</p>}
        {error && <p className="err">{error instanceof Error ? error.message : 'failed'}</p>}

        {!isLoading && requests.length === 0 && (
          <p className="muted">
            nothing yet — try{' '}
            <code>
              curl -X POST localhost:8080/e/{inbox.slug}/webhook -d '{'{'}"hi":1{'}'}'
            </code>
          </p>
        )}

        <ul className="captures">
          {requests.map((r) => (
            <li key={r.id}>
              <span className={`method m-${r.method.toLowerCase()}`}>{r.method}</span>
              <span className="path">
                {r.path}
                {r.query && <span className="muted">?{r.query}</span>}
              </span>
              <span className="muted size">
                {r.body_size} B{r.body_truncated && ' (truncated)'}
              </span>
              <span className="muted when">{new Date(r.received_at).toLocaleTimeString()}</span>
            </li>
          ))}
        </ul>
      </section>

      <footer>Phase 2, unit 5 — the list is live. The detail pane lands in unit 16.</footer>
    </main>
  )
}

function Header() {
  return (
    <>
      <h1>hooklens</h1>
      <p className="tagline">
        see what your webhooks actually send — then send them to localhost
      </p>
    </>
  )
}
