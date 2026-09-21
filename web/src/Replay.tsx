import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { keys, replayRequest, type Inbox } from './lib/api'
import { formatMs } from './lib/delivery'

/**
 * Resend a capture, optionally with an edited body.
 *
 * See docs/learn/27-replay-and-ssrf.md for why an arbitrary URL is allowed
 * at all and what guards it.
 */
export function Replay({ id, inbox, bodyB64 }: { id: string; inbox: Inbox; bodyB64: string }) {
  const [open, setOpen] = useState(false)
  const [target, setTarget] = useState('tunnel')
  const [edited, setEdited] = useState<string | null>(null)

  const qc = useQueryClient()
  const replay = useMutation({
    mutationFn: () =>
      replayRequest(id, inbox.token, {
        target,
        // Only sent when the body was actually touched. Sending the
        // unchanged body would mark every replay as "edited" and trigger
        // the signature warning on replays where the signature is fine.
        bodyB64: edited === null ? undefined : btoa(edited),
      }),
    onSuccess: (res) => {
      // A replay through the tunnel produces a NEW capture on the server.
      // Without this the list silently shows stale data -- mutations
      // invalidate nothing by default, which is the half of the
      // query/mutation split that is easy to forget.
      if (res.target === 'tunnel') {
        qc.invalidateQueries({ queryKey: keys.requests(inbox.slug) })
      }
    },
  })

  if (!open) {
    return (
      <button className="link" onClick={() => setOpen(true)}>
        replay this request
      </button>
    )
  }

  return (
    <section className="sig">
      <h3>replay</h3>

      <div className="row">
        <label className="tiny muted" htmlFor="replay-target">
          to
        </label>
        <input
          id="replay-target"
          className="secret"
          value={target}
          onChange={(e) => setTarget(e.target.value)}
          placeholder="tunnel, or https://…"
        />
        <button onClick={() => replay.mutate()} disabled={replay.isPending}>
          {replay.isPending ? 'sending…' : 'send'}
        </button>
        <button className="link" onClick={() => setOpen(false)}>
          close
        </button>
      </div>

      <p className="muted tiny">
        <code>tunnel</code> delivers to whatever <code>hooklens forward</code> is pointed
        at. A URL must be publicly routable — private and link-local addresses are refused,
        so this server cannot be used to reach networks on someone else&apos;s behalf.
      </p>

      <details
        onToggle={(e) => {
          // The editor is seeded from the original only when it is first
          // opened, so opening and closing it does not count as an edit.
          if ((e.target as HTMLDetailsElement).open && edited === null) {
            setEdited(safeAtob(bodyB64))
          }
        }}
      >
        <summary>edit the body first</summary>
        <textarea
          className="editor"
          rows={10}
          value={edited ?? ''}
          onChange={(e) => setEdited(e.target.value)}
          spellCheck={false}
        />
        {edited !== null && (
          <button className="link" onClick={() => setEdited(null)}>
            discard edits
          </button>
        )}
      </details>

      {replay.isError && (
        <p className="err">
          {replay.error instanceof Error ? replay.error.message : 'replay failed'}
        </p>
      )}

      {replay.data && (
        <div className={replay.data.replayed ? 'delivery ok' : 'delivery fail'}>
          {replay.data.replayed ? (
            <p>
              <strong>{replay.data.status}</strong> from {replay.data.target}
              {replay.data.elapsed_ms !== undefined && (
                <span className="muted"> in {formatMs(replay.data.elapsed_ms)}</span>
              )}
            </p>
          ) : (
            <p>{replay.data.error}</p>
          )}
          {replay.data.hint && <p className="muted">{replay.data.hint}</p>}
          {replay.data.signature_note && (
            <p className="muted">{replay.data.signature_note}</p>
          )}
          {replay.data.body_b64 && (
            <pre className="raw">{safeAtob(replay.data.body_b64)}</pre>
          )}
        </div>
      )}
    </section>
  )
}

/**
 * atob throws on anything that is not valid base64, and on bytes outside
 * Latin-1 it produces mojibake rather than failing. Both are possible here
 * -- a captured body is arbitrary bytes -- so the editor degrades to a
 * message rather than taking the pane down.
 */
function safeAtob(b64: string): string {
  try {
    return atob(b64)
  } catch {
    return '(this body is not editable as text)'
  }
}
