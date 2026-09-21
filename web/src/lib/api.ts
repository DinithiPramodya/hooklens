/**
 * The typed HTTP surface. Everything that talks to the server lives here, so
 * components never build URLs or remember to attach the token.
 */

export type Inbox = { slug: string; token: string }

/** One capture as the list endpoint and the stream both describe it. */
export type CaptureSummary = {
  id: string
  method: string
  path: string
  query: string
  body_size: number
  body_truncated: boolean
  header_count: number
  received_at: string
  declared_size?: number
  source_ip?: string
}

export type Header = { name: string; value: string }

/**
 * One capture in full.
 *
 * The body arrives base64-encoded, not as a string, because it is `bytea` on
 * the server: arbitrary bytes, often not valid UTF-8, and JSON strings must
 * be. Decoding and deciding how to render is the client's job -- see
 * `body.ts`.
 */
export type CaptureDetail = CaptureSummary & {
  headers: Header[]
  body_base64: string
}

export type Page = {
  inbox: string
  requests: CaptureSummary[]
  next_cursor?: string
}

export class ApiError extends Error {
  // Declared and assigned separately rather than as a constructor parameter
  // property (`constructor(readonly status: number)`). That shorthand emits
  // runtime assignments, so it is not erasable type syntax -- and this project
  // has `erasableSyntaxOnly` on, which is what lets Node run these files
  // directly by stripping types with no transform step.
  readonly status: number

  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function get<T>(path: string, token: string): Promise<T> {
  const resp = await fetch(path, {
    headers: { Authorization: `Bearer ${token}` },
  })
  if (!resp.ok) {
    // The server answers every failure as JSON -- including 404 on an
    // unmatched API path, which is what stops the SPA fallback returning HTML
    // here. Parsing defensively anyway: a proxy in front of us might not.
    let detail = `HTTP ${resp.status}`
    try {
      const body = (await resp.json()) as { error?: string }
      if (body.error) detail = body.error
    } catch {
      /* not JSON; keep the status */
    }
    throw new ApiError(resp.status, detail)
  }
  return (await resp.json()) as T
}

export function createInbox(name = 'browser'): Promise<Inbox & { urls: Record<string, string> }> {
  return fetch('/api/endpoints', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name }),
  }).then(async (r) => {
    if (!r.ok) throw new ApiError(r.status, `HTTP ${r.status}`)
    return r.json()
  })
}

export function listRequests(slug: string, token: string, after?: string): Promise<Page> {
  const qs = new URLSearchParams({ limit: '50' })
  if (after) qs.set('after', after)
  return get<Page>(`/api/endpoints/${slug}/requests?${qs}`, token)
}

export function getRequest(id: string, token: string): Promise<CaptureDetail> {
  // Note the shape: /api/requests/{id}, not /api/endpoints/{slug}/requests/{id}.
  // The id is a UUIDv7 and the token authorises it, so the slug would be
  // decoration -- and a second path segment the server would have to check
  // agrees with the first, which is one more way to get authorisation wrong.
  return get<CaptureDetail>(`/api/requests/${id}`, token)
}

/**
 * Query keys, in one place.
 *
 * The key IS the cache identity, so a key built inline in two components can
 * drift by one variable and silently share an entry between two inboxes. A
 * single factory makes that impossible.
 */
export const keys = {
  requests: (slug: string) => ['requests', slug] as const,
  request: (id: string) => ['request', id] as const,
}
