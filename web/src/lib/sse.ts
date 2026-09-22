/**
 * A Server-Sent Events client built on fetch, not EventSource.
 *
 * WHY NOT EventSource, given the brief argues its automatic reconnection is
 * SSE's main advantage?
 *
 * Because EventSource cannot send request headers. There is no API for it and
 * there never has been. Our streams are authenticated with an Authorization
 * bearer token like every other read, so EventSource would force one of:
 *
 *   - the token in the query string, which puts a long-lived secret into
 *     access logs, proxy logs and browser history -- the exact leak that makes
 *     people distrust capability URLs, and something unit 08 explicitly
 *     refused;
 *   - a cookie, which means CSRF defences for a system that currently has none
 *     and does not need them;
 *   - a short-lived ticket endpoint, which is a real pattern but is a whole
 *     auth mechanism added to satisfy one browser API.
 *
 * So we keep the SSE *protocol* -- simple, text, one-directional, readable with
 * curl -- and give up the EventSource *convenience*, reimplementing the
 * reconnection below. That is about thirty lines, and it buys a uniform auth
 * model. See docs/learn/13-server-sent-events.md.
 */

export type SSEEvent = { event: string; data: string; id?: string }

export type StreamHandlers = {
  onEvent: (e: SSEEvent) => void
  onOpen?: () => void
  onError?: (err: unknown) => void
  /** The server rejected the credential; the stream has stopped for good. */
  onFatal?: (status: number) => void
}

/**
 * Whether an HTTP status means retrying can never succeed.
 *
 * 401 and 403 say "this token is not accepted" -- the inbox was deleted, the
 * database was reset, or the token is wrong -- and waiting changes none of
 * that. Retrying them forever showed "retrying" beside a dead inbox
 * indefinitely: a promise of recovery that could not come.
 *
 * Deliberately narrow. Everything else -- network errors, 5xx, a dropped
 * stream -- may heal, and misclassifying one of THOSE as permanent would log
 * every open tab out on a server restart, which is worse than the bug this
 * fixes. The same split as the CLI's CloseError.Permanent().
 * See docs/learn/40-dead-inboxes-and-handoff-links.md.
 */
export function isPermanentStatus(status: number): boolean {
  return status === 401 || status === 403
}

/**
 * Parses the SSE wire format out of a stream of text chunks.
 *
 * Kept separate from the transport because the fiddly part is purely textual:
 * a chunk boundary can fall anywhere, including mid-event and mid-line, so the
 * parser has to hold a buffer across calls. A parser that assumed one chunk is
 * one event would work perfectly in development and corrupt events under load.
 */
export class SSEParser {
  private buffer = ''

  /** Feed a chunk; returns whatever complete events it completed. */
  push(chunk: string): SSEEvent[] {
    this.buffer += chunk
    const out: SSEEvent[] = []

    // Events are separated by a blank line. Anything after the last separator
    // is a partial event and stays in the buffer for the next chunk.
    let sep: number
    while ((sep = this.buffer.indexOf('\n\n')) !== -1) {
      const raw = this.buffer.slice(0, sep)
      this.buffer = this.buffer.slice(sep + 2)

      const parsed = parseFrame(raw)
      if (parsed) out.push(parsed)
    }
    return out
  }
}

function parseFrame(raw: string): SSEEvent | null {
  let event = 'message' // the spec's default when no event: field is present
  let id: string | undefined
  const dataLines: string[] = []

  for (const line of raw.split('\n')) {
    // A line starting with ':' is a comment. Our heartbeats are exactly this:
    // ignored by the parser, but they are bytes on the wire, which is the
    // whole point.
    if (line.startsWith(':')) continue

    const colon = line.indexOf(':')
    const field = colon === -1 ? line : line.slice(0, colon)
    // One optional space after the colon is part of the format, not the value.
    let value = colon === -1 ? '' : line.slice(colon + 1)
    if (value.startsWith(' ')) value = value.slice(1)

    switch (field) {
      case 'event':
        event = value
        break
      case 'data':
        // Multiple data: lines are rejoined with newlines -- the inverse of
        // what the server does when splitting a multi-line payload.
        dataLines.push(value)
        break
      case 'id':
        id = value
        break
      // 'retry' is deliberately ignored: we control our own backoff below.
    }
  }

  if (dataLines.length === 0) return null // a comment-only frame
  return { event, data: dataLines.join('\n'), id }
}

/**
 * Opens an authenticated event stream and keeps it open.
 *
 * Returns a function that closes it. Reconnects with exponential backoff and
 * jitter -- the jitter matters because without it every open tab reconnects at
 * the same instant after a server restart, which is the thundering herd that
 * turns a brief blip into a sustained one.
 */
export function streamEvents(
  url: string,
  token: string,
  handlers: StreamHandlers,
): () => void {
  const controller = new AbortController()
  let closed = false
  let attempt = 0

  async function connect() {
    while (!closed) {
      try {
        const resp = await fetch(url, {
          headers: {
            Authorization: `Bearer ${token}`,
            Accept: 'text/event-stream',
          },
          signal: controller.signal,
        })

        if (isPermanentStatus(resp.status)) {
          // Stop, and say so. No backoff, no further requests.
          closed = true
          handlers.onFatal?.(resp.status)
          return
        }
        if (!resp.ok || !resp.body) {
          throw new Error(`HTTP ${resp.status}`)
        }

        attempt = 0 // a successful connection resets the backoff
        handlers.onOpen?.()

        const reader = resp.body.pipeThrough(new TextDecoderStream()).getReader()
        const parser = new SSEParser()

        for (;;) {
          const { done, value } = await reader.read()
          // `done` means the server closed the stream. That is not an error,
          // but it is not normal either -- fall through to the reconnect.
          if (done) break
          for (const e of parser.push(value)) handlers.onEvent(e)
        }
      } catch (err) {
        // An aborted fetch is us closing deliberately, not a failure.
        if (closed || controller.signal.aborted) return
        handlers.onError?.(err)
      }

      if (closed) return

      // Exponential backoff, capped, with jitter.
      const base = Math.min(1000 * 2 ** attempt, 30_000)
      const delay = base * (0.5 + Math.random() / 2)
      attempt++
      await new Promise((r) => setTimeout(r, delay))
    }
  }

  void connect()

  return () => {
    closed = true
    controller.abort()
  }
}
