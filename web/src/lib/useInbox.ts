import { useCallback, useEffect, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { keys, listRequests, type CaptureSummary, type Inbox, type Page } from './api'
import { streamEvents } from './sse'

const STORAGE_KEY = 'hooklens.inbox'

/**
 * The inbox itself is CLIENT state: it lives only in this browser and nothing
 * else can change it. The token in particular cannot be recovered from the
 * server -- it is stored there only as a SHA-256 -- so losing it on refresh
 * would mean losing the inbox.
 */
export function useStoredInbox() {
  const [inbox, setInboxState] = useState<Inbox | null>(() => {
    try {
      const raw = localStorage.getItem(STORAGE_KEY)
      return raw ? (JSON.parse(raw) as Inbox) : null
    } catch {
      return null
    }
  })

  const setInbox = useCallback((next: Inbox | null) => {
    try {
      if (next) localStorage.setItem(STORAGE_KEY, JSON.stringify(next))
      else localStorage.removeItem(STORAGE_KEY)
    } catch {
      /* private mode, quota, blocked storage -- the app still works, it just
         forgets on refresh. Worth degrading rather than failing. */
    }
    setInboxState(next)
  }, [])

  return { inbox, setInbox }
}

/**
 * The capture list is SERVER state: a copy of rows that live in Postgres,
 * which anything can add to at any moment.
 */
export function useRequests(inbox: Inbox | null) {
  return useQuery({
    // slug is in the key, so switching inboxes reads a different cache entry
    // rather than briefly showing the previous inbox's captures.
    queryKey: keys.requests(inbox?.slug ?? ''),
    queryFn: () => listRequests(inbox!.slug, inbox!.token),
    enabled: inbox !== null,
    // The stream keeps this current, so background refetching on focus or
    // reconnect is redundant chatter. The initial load is the only fetch we
    // actually need; everything after arrives by SSE.
    staleTime: Infinity,
    refetchOnWindowFocus: false,
  })
}

export type StreamStatus = 'idle' | 'connecting' | 'live' | 'retrying'

/**
 * Subscribes to the inbox's event stream and writes arriving captures straight
 * into the query cache.
 *
 * setQueryData rather than invalidateQueries, deliberately. Invalidating would
 * refetch the whole list to learn what the event just told us -- a round trip
 * per webhook, and a visible delay before the row appears. Writing directly is
 * safe here in a way an optimistic update is not: this is not a guess about
 * what the server will do, it is the server reporting what it already did.
 */
export function useLiveCaptures(inbox: Inbox | null) {
  const qc = useQueryClient()
  const [status, setStatus] = useState<StreamStatus>('idle')
  const [dropped, setDropped] = useState(0)

  useEffect(() => {
    if (!inbox) {
      setStatus('idle')
      return
    }
    setStatus('connecting')
    setDropped(0)

    // Returning this closes the stream on unmount or inbox change. Without it
    // StrictMode's double-mount leaves an orphan connection feeding the same
    // cache, and every capture appears twice.
    return streamEvents(`/api/endpoints/${inbox.slug}/stream`, inbox.token, {
      onOpen: () => setStatus('live'),
      onError: () => setStatus('retrying'),
      onEvent: (e) => {
        if (e.event === 'dropped') {
          // The broker discarded events because this tab fell behind. Surfaced
          // rather than swallowed: a silently incomplete list is worse than a
          // visible gap, because the user cannot tell which they are looking at.
          try {
            setDropped((n) => n + (JSON.parse(e.data) as { count: number }).count)
          } catch {
            setDropped((n) => n + 1)
          }
          return
        }
        if (e.event !== 'capture') return

        let capture: CaptureSummary
        try {
          capture = JSON.parse(e.data) as CaptureSummary
        } catch {
          return
        }

        qc.setQueryData<Page>(keys.requests(inbox.slug), (prev) => {
          if (!prev) return prev // nothing cached yet; the initial fetch will include it
          // Guard against duplicates. A reconnect can replay, and the initial
          // fetch can race an event for the same row -- without this the same
          // capture appears twice and looks like the provider double-sent,
          // which is the exact confusion this tool exists to remove.
          if (prev.requests.some((r) => r.id === capture.id)) return prev
          return { ...prev, requests: [capture, ...prev.requests] }
        })
      },
    })
  }, [inbox, qc])

  return { status, dropped }
}
