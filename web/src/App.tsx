import { useState } from 'react'
import { createInbox, type CaptureSummary } from './lib/api'
import { useLiveCaptures, useRequests, useStoredInbox } from './lib/useInbox'
import { Detail } from './Detail'
import { deliveryBadge, deliveryOf } from './lib/delivery'
import { Diff } from './Diff'

export default function App() {
  const { inbox, setInbox } = useStoredInbox()
  const { status, dropped } = useLiveCaptures(inbox)
  const { data, isLoading, error } = useRequests(inbox)
  const [creating, setCreating] = useState(false)
  const [createError, setCreateError] = useState<string | null>(null)
  // Which row is open. The id, not the capture object -- the list is a cache
  // that the stream rewrites, and holding a copy of a row here would mean two
  // sources of truth for the same capture.
  const [selected, setSelected] = useState<string | null>(null)
  // The second capture in a comparison. Separate from `selected` because a
  // diff needs both and the first one is whatever is already open -- which
  // is the interaction people expect from a file browser.
  const [compareWith, setCompareWith] = useState<string | null>(null)

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

  // Derived from the most recent capture rather than from a live signal,
  // because there is no "is a tunnel attached" endpoint -- and adding one
  // would be a second source of truth that could disagree with the rows.
  // The newest capture is the freshest evidence available.
  const noTunnel = requests.length > 0 && deliveryOf(requests[0]).kind === 'none'

  return (
    <main>
      <Header />

      <section>
        <div className="row">
          <code className="url">/e/{inbox.slug}/</code>
          <span className={`dot dot-${status}`} aria-hidden="true" />
          <span className="muted">{status}</span>
          <button
            className="link"
            onClick={() => {
              setSelected(null)
              setInbox(null)
            }}
          >
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

        {/* Said once, not on every row. "No tunnel" is the normal state of an
            inbox used for inspection, so repeating it per capture would paint
            a working system as broken. */}
        {/* The line is reserved whenever there are two captures to compare,
            and only its TEXT comes and goes. Rendering the paragraph itself
            conditionally made it appear on the click that selected a row --
            pushing every row down ~38px under the cursor, so a quick second
            click landed on the wrong capture. Found in the demo recording,
            where the click marker ended up on the row that was NOT selected. */}
        {requests.length > 1 && (
          <p
            className="muted tiny"
            style={{ visibility: selected && !compareWith ? 'visible' : 'hidden' }}
            aria-hidden={!(selected && !compareWith)}
          >
            shift-click another capture to compare it with this one
          </p>
        )}

        {noTunnel && (
          <p className="muted notice">
            captures are being stored but not forwarded — run{' '}
            <code>hooklens forward --to localhost:3000</code> to deliver them to a local app
          </p>
        )}

        <div className="panes">
          <ul className="captures">
            {requests.map((r) => (
              <li
                key={r.id}
                className={
                  r.id === selected ? 'sel' : r.id === compareWith ? 'cmp' : undefined
                }
              >
                <button
                  className="rowbtn"
                  onClick={(e) => {
                    // Shift-click picks the second side of a comparison.
                    // A modifier rather than a mode: selecting two things
                    // is already a familiar gesture, and a "compare mode"
                    // toggle would be one more piece of state to explain.
                    if (e.shiftKey && selected && r.id !== selected) {
                      setCompareWith(r.id)
                      return
                    }
                    setCompareWith(null)
                    setSelected(r.id)
                  }}
                >
                  <span className={`method m-${r.method.toLowerCase()}`}>{r.method}</span>
                  <span className="path">
                    {r.path}
                    {r.query && <span className="muted">?{r.query}</span>}
                  </span>
                  <span className="muted size">
                    {r.body_size} B{r.body_truncated && ' (truncated)'}
                  </span>
                  <DeliveryBadge capture={r} />
                  <span className="muted when">
                    {new Date(r.received_at).toLocaleTimeString()}
                  </span>
                </button>
              </li>
            ))}
          </ul>

          {/* Keyed by id so selecting a different capture MOUNTS A NEW Detail
              rather than updating the old one. Without the key, React reuses
              the component and its state -- so the tab you were on and every
              expanded node in the tree would carry over onto an unrelated
              request, showing one capture's open branches over another's
              data. */}
          {selected && inbox && (
            <div className="rightpane">
              {compareWith ? (
                <Diff
                  left={selected}
                  right={compareWith}
                  inbox={inbox}
                  onClose={() => setCompareWith(null)}
                />
              ) : (
                <Detail key={selected} id={selected} inbox={inbox} />
              )}
            </div>
          )}
        </div>
      </section>

      {/* Was a build-phase status line, which went stale three phases running.
          This says something a user needs instead: shift-click is the only way
          to reach the diff, and nothing else on the page mentions it. */}
      <footer>Click a capture to inspect it · shift-click a second one to compare the two.</footer>
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

/**
 * The per-row delivery indicator.
 *
 * Renders nothing at all when forwarding was never attempted. An inbox with
 * no tunnel would otherwise repeat the same notice on every line; it is said
 * once, above the list.
 */
function DeliveryBadge({ capture }: { capture: CaptureSummary }) {
  const badge = deliveryBadge(deliveryOf(capture))
  if (!badge) return null
  return <span className={`badge ${badge.cls}`}>{badge.text}</span>
}
