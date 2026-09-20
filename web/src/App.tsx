import { useCallback, useEffect, useRef, useState } from 'react'
import { streamEvents, type SSEEvent } from './lib/sse'

type Inbox = { slug: string; token: string }

// localStorage, deliberately, and only until unit 15 designs this properly.
// The token cannot be recovered from the server -- it is stored only as a
// SHA-256 -- so losing it on refresh would mean losing the inbox.
const STORAGE_KEY = 'hooklens.inbox'

function loadInbox(): Inbox | null {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    return raw ? (JSON.parse(raw) as Inbox) : null
  } catch {
    return null
  }
}

export default function App() {
  const [inbox, setInbox] = useState<Inbox | null>(loadInbox)
  const [status, setStatus] = useState<'idle' | 'connecting' | 'live' | 'retrying'>('idle')
  const [events, setEvents] = useState<Array<SSEEvent & { at: string }>>([])
  const [error, setError] = useState<string | null>(null)

  const createInbox = useCallback(async () => {
    setError(null)
    try {
      const resp = await fetch('/api/endpoints', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: 'browser' }),
      })
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`)
      const data = (await resp.json()) as Inbox
      const next = { slug: data.slug, token: data.token }
      localStorage.setItem(STORAGE_KEY, JSON.stringify(next))
      setInbox(next)
      setEvents([])
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }, [])

  // One stream per inbox, torn down on change or unmount.
  //
  // The cleanup return is load-bearing: without it, StrictMode's deliberate
  // double-mount in development leaves an orphaned connection open, and every
  // inbox change would add another. They would all keep receiving, and the
  // event list would show duplicates from connections nobody can see.
  const streamRef = useRef<(() => void) | null>(null)
  useEffect(() => {
    if (!inbox) return

    setStatus('connecting')
    const close = streamEvents(
      `/api/endpoints/${inbox.slug}/stream`,
      inbox.token,
      {
        onOpen: () => setStatus('live'),
        onError: () => setStatus('retrying'),
        onEvent: (e) =>
          setEvents((prev) =>
            [{ ...e, at: new Date().toLocaleTimeString() }, ...prev].slice(0, 50),
          ),
      },
    )
    streamRef.current = close
    return close
  }, [inbox])

  return (
    <main>
      <h1>hooklens</h1>
      <p className="tagline">
        see what your webhooks actually send — then send them to localhost
      </p>

      {!inbox ? (
        <section>
          <h2>get started</h2>
          <button onClick={createInbox}>Create an inbox</button>
          {error && <p className="err">{error}</p>}
        </section>
      ) : (
        <>
          <section>
            <h2>your inbox</h2>
            <p>
              <code>/e/{inbox.slug}/</code>
            </p>
            <p className={status === 'live' ? 'ok' : status === 'retrying' ? 'err' : 'muted'}>
              stream: {status}
            </p>
          </section>

          <section>
            <h2>events ({events.length})</h2>
            {events.length === 0 && <p className="muted">waiting…</p>}
            <ul className="events">
              {events.map((e, i) => (
                <li key={i}>
                  <span className="muted">{e.at}</span> <strong>{e.event}</strong>{' '}
                  <code>{e.data}</code>
                </li>
              ))}
            </ul>
          </section>
        </>
      )}

      <footer>
        Phase 2, unit 3 — the stream is live. Captures start flowing through it in unit 14.
      </footer>
    </main>
  )
}
