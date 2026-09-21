import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { diffRequests, type DiffChange, type Inbox } from './lib/api'

/**
 * Compare two captures.
 *
 * A query rather than a mutation, unlike verify and replay: a diff is a
 * read, it is idempotent, and caching it is free. The distinction is not
 * bookkeeping -- see docs/learn/29-mutations-and-secrets.md.
 */
export function Diff({
  left,
  right,
  inbox,
  onClose,
}: {
  left: string
  right: string
  inbox: Inbox
  onClose: () => void
}) {
  const [volatile_, setVolatile] = useState(false)

  const q = useQuery({
    queryKey: ['diff', left, right, volatile_],
    queryFn: () => diffRequests(left, right, inbox.token, volatile_),
    // A diff of two immutable captures cannot go stale.
    staleTime: Infinity,
    refetchOnWindowFocus: false,
  })

  return (
    <section className="detail">
      <div className="row dtitle">
        <h3>diff</h3>
        <button className="link" onClick={onClose}>
          close
        </button>
      </div>

      {q.isLoading && <p className="muted">comparing…</p>}
      {q.error && (
        <p className="err">{q.error instanceof Error ? q.error.message : 'failed'}</p>
      )}

      {q.data && (
        <>
          {q.data.identical ? (
            <p className="delivery ok">
              identical
              {q.data.volatile_hidden && (
                <span className="muted"> (ignoring headers that change every request)</span>
              )}
            </p>
          ) : (
            <>
              <Section title="request line" changes={q.data.request} />
              <Section title="headers" changes={q.data.headers} />
              <Section title="body" changes={q.data.body} note={q.data.body_note} />
            </>
          )}

          {/* The toggle exists because a diff that silently hides fields is
              a diff that lies -- and the signature may be exactly what
              somebody is investigating. */}
          <label className="tiny muted toggle">
            <input
              type="checkbox"
              checked={volatile_}
              onChange={(e) => setVolatile(e.target.checked)}
            />{' '}
            show headers that differ on every request ({q.data.volatile_headers.length}{' '}
            hidden by default)
          </label>
        </>
      )}
    </section>
  )
}

function Section({
  title,
  changes,
  note,
}: {
  title: string
  changes: DiffChange[] | null
  note?: string
}) {
  if (!changes || changes.length === 0) {
    if (!note) return null
    return (
      <div className="diffsec">
        <h4>{title}</h4>
        <p className="muted tiny">{note}</p>
      </div>
    )
  }

  return (
    <div className="diffsec">
      <h4>
        {title} <span className="muted">({changes.length})</span>
      </h4>
      {note && <p className="muted tiny">{note}</p>}
      <table className="difftable">
        <tbody>
          {changes.map((c, i) => (
            // Index as key: the list is derived from one immutable
            // comparison and never reorders.
            <tr key={i} className={`d-${c.op}`}>
              <td className="dpath">{c.path}</td>
              <td className="dop">{c.op}</td>
              <td className="dval">
                {c.op !== 'added' && <del>{c.old}</del>}
                {c.op === 'changed' && ' → '}
                {c.op !== 'removed' && <ins>{c.new}</ins>}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
