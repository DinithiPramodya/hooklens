import { useMemo, useState } from 'react'
import type { CaptureDetail, Inbox } from './lib/api'
import { classifyBody, decodeBase64, formatBytes, hexDump } from './lib/body'
import { useRequest } from './lib/useInbox'
import { JsonTree } from './JsonTree'

type Tab = 'body' | 'headers' | 'raw'

export function Detail({ id, inbox }: { id: string; inbox: Inbox }) {
  const { data, isLoading, error } = useRequest(id, inbox)
  const [tab, setTab] = useState<Tab>('body')

  if (isLoading) return <p className="muted">loading…</p>
  if (error) return <p className="err">{error instanceof Error ? error.message : 'failed'}</p>
  if (!data) return null

  return (
    <div className="detail">
      <div className="row dtitle">
        <span className={`method m-${data.method.toLowerCase()}`}>{data.method}</span>
        <code className="path">
          {data.path}
          {data.query && <span className="muted">?{data.query}</span>}
        </code>
      </div>

      <p className="muted meta">
        {new Date(data.received_at).toLocaleString()} · {formatBytes(data.body_size)}
        {data.body_truncated && ' (truncated)'} · {data.header_count} headers
        {data.source_ip && ` · from ${data.source_ip}`}
      </p>

      {/* The one warning that must never be missed: what is shown is not what
          arrived. Everything below it -- the tree, the hex, the size -- is
          describing a prefix. */}
      {data.body_truncated && (
        <p className="err">
          body truncated at the ingest limit
          {data.declared_size !== undefined &&
            ` — the sender declared ${formatBytes(data.declared_size)}`}
        </p>
      )}

      <div className="tabs" role="tablist">
        {(['body', 'headers', 'raw'] as Tab[]).map((t) => (
          <button
            key={t}
            role="tab"
            aria-selected={tab === t}
            className={tab === t ? 'tab on' : 'tab'}
            onClick={() => setTab(t)}
          >
            {t}
          </button>
        ))}
      </div>

      {tab === 'body' && <Body b64={data.body_base64} />}
      {tab === 'headers' && <Headers headers={data.headers} />}
      {tab === 'raw' && <Raw req={data} />}
    </div>
  )
}

function Body({ b64 }: { b64: string }) {
  // Decoding and classifying is pure work over up to a megabyte, and it does
  // not depend on which tab is open or on any state that changes while
  // reading. Memoised on the base64 so switching tabs does not redo it.
  const view = useMemo(() => classifyBody(decodeBase64(b64)), [b64])

  switch (view.kind) {
    case 'empty':
      return <p className="muted">no body</p>

    case 'json':
      return <JsonTree value={view.value} />

    case 'text':
      // Not JSON -- which very often means malformed JSON, and that is
      // frequently the bug being hunted. Shown verbatim rather than as a
      // parse error, because the exact bytes are the evidence.
      return <pre className="raw">{view.text}</pre>

    case 'binary':
      return (
        <>
          <p className="muted">
            binary — {formatBytes(view.bytes.length)}, not valid UTF-8
          </p>
          <pre className="raw">{hexDump(view.bytes)}</pre>
        </>
      )
  }
}

function Headers({ headers }: { headers: { name: string; value: string }[] }) {
  if (headers.length === 0) return <p className="muted">no headers</p>
  return (
    <table className="headers">
      <tbody>
        {/* Index as key, not name: headers legitimately repeat (Set-Cookie,
            and any signature scheme that sends several candidates), and the
            order they arrived in is itself evidence. Deduplicating by name --
            which a name key would quietly encourage -- would destroy exactly
            what this pane exists to show. */}
        {headers.map((h, i) => (
          <tr key={i}>
            <td className="hname">{h.name}</td>
            <td className="hval">{h.value}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

function Raw({ req }: { req: CaptureDetail }) {
  // Reconstructs what came off the wire: request line, headers, blank line,
  // body. Not the literal bytes -- the HTTP version and the original casing
  // are gone by the time Go has parsed it -- but the shape a person recognises
  // and can paste into a bug report.
  const text = useMemo(() => {
    const line = `${req.method} ${req.path}${req.query ? '?' + req.query : ''} HTTP/1.1`
    const headers = req.headers.map((h) => `${h.name}: ${h.value}`).join('\n')
    const view = classifyBody(decodeBase64(req.body_base64))
    const body =
      view.kind === 'empty'
        ? ''
        : view.kind === 'binary'
          ? hexDump(view.bytes)
          : view.text
    return `${line}\n${headers}\n\n${body}`
  }, [req])

  return <pre className="raw">{text}</pre>
}
