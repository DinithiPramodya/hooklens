/**
 * How a capture's forwarding outcome is described to a person.
 *
 * Kept out of the components, and out of `api.ts`, because it is neither
 * transport nor rendering: it is the one place the server's machine codes
 * become sentences. See docs/learn/25-showing-delivery.md.
 */

import type { CaptureSummary } from './api'

/**
 * Three states, not two, and the third is the most common one.
 *
 * `none` means forwarding was never attempted -- no tunnel was connected --
 * which is the normal state of an inbox being used purely for inspection.
 * Rendering it as a failure paints a wall of red over a system working
 * correctly; rendering it as success tells a developer whose CLI has died
 * that everything is fine. It has to be its own thing.
 */
export type Delivery =
  | { kind: 'none' }
  | { kind: 'delivered'; status: number; ms?: number }
  | { kind: 'failed'; code: string; label: string; detail: string; ms?: number }

/** Machine code -> what a person needs to read, and what to do about it. */
const FAILURES: Record<string, { label: string; detail: string }> = {
  no_tunnel: {
    label: 'not forwarded',
    detail: 'No tunnel was connected. Run `hooklens forward --to localhost:3000`.',
  },
  unreachable: {
    label: 'unreachable',
    detail: 'The tunnel is up but your local app refused the connection. Is it running?',
  },
  timeout: {
    label: 'timed out',
    detail: 'Your local app was reached but did not answer in time.',
  },
  disconnected: {
    label: 'tunnel dropped',
    detail: 'The tunnel disconnected while this request was in flight.',
  },
  overloaded: {
    label: 'overloaded',
    detail: 'The tunnel was already at its in-flight limit and no slot came free.',
  },
  too_large: {
    label: 'too large',
    detail:
      'The body exceeded the capture limit, so it was stored but not forwarded — ' +
      'a truncated body would have reached your app as corrupt data. Raise HOOKLENS_MAX_BODY.',
  },
  protocol: {
    label: 'protocol error',
    detail: 'The tunnel misbehaved. This is a bug in hooklens; the log has more.',
  },
}

export function deliveryOf(r: CaptureSummary): Delivery {
  if (r.forward_status !== undefined) {
    return { kind: 'delivered', status: r.forward_status, ms: r.forward_ms }
  }
  if (r.forward_error !== undefined) {
    const known = FAILURES[r.forward_error]
    return {
      kind: 'failed',
      code: r.forward_error,
      // An unknown code still renders as itself rather than as nothing. A
      // server newer than this bundle is a real situation, and "" would be
      // indistinguishable from "not attempted" -- the exact collapse this
      // module exists to prevent.
      label: known?.label ?? r.forward_error,
      detail: known?.detail ?? 'The server reported a failure this version does not recognise.',
      ms: r.forward_ms,
    }
  }
  return { kind: 'none' }
}

/**
 * The short form for a list row.
 *
 * Text, not only a colour: colour alone fails for the colour-blind and
 * vanishes from a screenshot pasted into a bug report, which is exactly where
 * these end up.
 */
export function deliveryBadge(d: Delivery): { text: string; cls: string } | null {
  switch (d.kind) {
    case 'none':
      // Nothing on the row. An inbox with no tunnel would otherwise repeat
      // the same notice on every line; it is said once, at the top.
      return null
    case 'delivered':
      return {
        text: String(d.status),
        cls: d.status >= 500 ? 'd-appfail' : d.status >= 400 ? 'd-appwarn' : 'd-ok',
      }
    case 'failed':
      return { text: d.label, cls: 'd-fail' }
  }
}

export function formatMs(ms?: number): string {
  if (ms === undefined) return ''
  if (ms < 1000) return `${ms}ms`
  return `${(ms / 1000).toFixed(1)}s`
}
