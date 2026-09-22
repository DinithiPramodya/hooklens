import { useCallback, useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { getRequest, keys, listRequests, type CaptureDetail, type CaptureSummary, type Inbox, type Page } from './api'
import { applyDelivery, withDelivery, type DeliveryEvent } from './delivery'
import { handoffDecision, hashHasToken, inboxFromHash } from './handoff'
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

  // An inbox handed over by `hooklens forward`'s inspect link, waiting for the
  // person to decide, because adopting it would forget a different inbox.
  const [pending, setPending] = useState<Inbox | null>(null)

  // The current inbox, readable from the hashchange listener below without
  // re-subscribing it on every change (a stale closure would compare the link
  // against whatever inbox was stored when the listener was attached).
  const inboxRef = useRef(inbox)
  inboxRef.current = inbox

  // Read the link's fragment, then take the token OUT of the address bar
  // immediately -- replaceState rewrites the current history entry instead of
  // adding one -- so it is not left on screen, bookmarked, or shared by
  // copying the URL. See lib/handoff.ts.
  //
  // On load AND on `hashchange`. Opening the link in a tab where hooklens is
  // already open changes only the part after `#`, which browsers treat as a
  // jump within the same page: no reload, no remount, so a mount-only read
  // never saw it -- and the token stayed in the address bar. Found by opening
  // the link during manual verification, not by a test.
  useEffect(() => {
    const handle = () => {
      const hash = window.location.hash
      const linked = inboxFromHash(hash)
      if (hashHasToken(hash)) {
        window.history.replaceState(null, '', window.location.pathname + window.location.search)
      }
      switch (handoffDecision(inboxRef.current, linked)) {
        case 'adopt':
          setInbox(linked)
          break
        case 'ask':
          setPending(linked)
          break
      }
    }
    handle()
    window.addEventListener('hashchange', handle)
    return () => window.removeEventListener('hashchange', handle)
  }, [setInbox])

  const acceptPending = useCallback(() => {
    if (pending) setInbox(pending)
    setPending(null)
  }, [pending, setInbox])

  const dismissPending = useCallback(() => setPending(null), [])

  return { inbox, setInbox, pending, acceptPending, dismissPending }
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

/**
 * One capture in full, fetched on demand when a row is selected.
 *
 * `staleTime: Infinity` is not a tuning choice here, it is a fact about the
 * data: the captured request itself is immutable, so refetching could only
 * ever return the same bytes. The one exception is the forwarding outcome,
 * which is recorded AFTER the capture -- and it is not left to go stale: the
 * stream's `delivery` event patches this cache entry directly (see
 * useLiveCaptures). Caching forever is correct because something else keeps
 * the one mutable part current.
 */
export function useRequest(id: string | null, inbox: Inbox | null) {
  return useQuery({
    queryKey: keys.request(id ?? ''),
    queryFn: () => getRequest(id!, inbox!.token),
    enabled: id !== null && inbox !== null,
    staleTime: Infinity,
    refetchOnWindowFocus: false,
  })
}

// 'gone' is terminal: the server rejected this inbox's token, so the stream has
// stopped rather than retrying something that cannot succeed.
export type StreamStatus = 'idle' | 'connecting' | 'live' | 'retrying' | 'gone'

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
      onFatal: () => setStatus('gone'),
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
        if (e.event === 'delivery') {
          // A forward's outcome, sent after the capture's own event because
          // the capture is broadcast before forwarding starts. Without this
          // the row stayed "not attempted" until a reload, under a notice
          // telling the user to start the tunnel they were already running.
          let d: DeliveryEvent
          try {
            d = JSON.parse(e.data) as DeliveryEvent
          } catch {
            return
          }
          qc.setQueryData<Page>(keys.requests(inbox.slug), (prev) =>
            prev ? { ...prev, requests: applyDelivery(prev.requests, d) } : prev,
          )
          // The detail pane has its own cache entry for an open capture.
          qc.setQueryData<CaptureDetail>(keys.request(d.id), (prev) =>
            prev ? withDelivery(prev, d) : prev,
          )
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
