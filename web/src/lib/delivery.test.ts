import { test } from 'node:test'
import assert from 'node:assert/strict'

import { deliveryBadge, deliveryOf, formatMs } from './delivery.ts'
import type { CaptureSummary } from './api.ts'

function capture(extra: Partial<CaptureSummary> = {}): CaptureSummary {
  return {
    id: 'x',
    method: 'POST',
    path: '/hook',
    query: '',
    body_size: 10,
    body_truncated: false,
    header_count: 3,
    received_at: new Date().toISOString(),
    ...extra,
  }
}

// The central claim of the unit: three states, and the third is not a
// failure. Collapsing it either way makes the interface lie -- red over a
// working inspection-only inbox, or green while a dead CLI delivers nothing.
test('a capture with no forwarding attempt is "none", not a failure', () => {
  const d = deliveryOf(capture())
  assert.equal(d.kind, 'none')
  assert.equal(deliveryBadge(d), null, 'a row with no attempt must show no badge')
})

test('a delivered capture carries the app status', () => {
  const d = deliveryOf(capture({ forward_status: 201, forward_ms: 42 }))
  assert.equal(d.kind, 'delivered')
  if (d.kind !== 'delivered') return
  assert.equal(d.status, 201)
  assert.equal(d.ms, 42)
  assert.deepEqual(deliveryBadge(d), { text: '201', cls: 'd-ok' })
})

// An app answering 500 was REACHED. It is not a delivery failure, and
// styling it as one would send a developer to check their tunnel when the
// problem is in their handler.
test('an app 5xx is delivered, styled apart from a delivery failure', () => {
  const d = deliveryOf(capture({ forward_status: 500 }))
  assert.equal(d.kind, 'delivered')
  const badge = deliveryBadge(d)
  assert.equal(badge?.text, '500')
  assert.equal(badge?.cls, 'd-appfail')
  assert.notEqual(badge?.cls, 'd-fail', 'an app error must not look like a delivery error')
})

test('an app 4xx gets its own styling', () => {
  assert.equal(deliveryBadge(deliveryOf(capture({ forward_status: 422 })))?.cls, 'd-appwarn')
})

test('every known failure code has a label and an actionable detail', () => {
  const codes = [
    'no_tunnel',
    'unreachable',
    'timeout',
    'disconnected',
    'overloaded',
    'too_large',
    'protocol',
  ]
  for (const code of codes) {
    const d = deliveryOf(capture({ forward_error: code }))
    assert.equal(d.kind, 'failed', code)
    if (d.kind !== 'failed') continue
    // Not "label differs from code" -- `unreachable` is a perfectly good
    // human label that happens to equal its code. What matters is that the
    // code was RECOGNISED, which the fallback detail text reveals.
    assert.ok(d.detail.length > 20, `${code} has no useful detail`)
    assert.ok(
      !d.detail.includes('does not recognise'),
      `${code} fell through to the unknown-code fallback`,
    )
    assert.ok(!d.label.includes('_'), `${code} was rendered as a raw machine code`)
    assert.equal(deliveryBadge(d)?.cls, 'd-fail')
  }
})

// A server newer than this bundle is a real situation. Falling back to "" or
// to 'none' would be the exact collapse this module exists to prevent.
test('an unknown failure code still renders as a failure', () => {
  const d = deliveryOf(capture({ forward_error: 'invented_later' }))
  assert.equal(d.kind, 'failed')
  if (d.kind !== 'failed') return
  assert.equal(d.label, 'invented_later')
  assert.ok(d.detail.length > 0)
})

// forward_status wins if both somehow appear. The database forbids it, but a
// UI that renders a contradiction as a blank is worse than one that picks.
test('a status takes precedence over an error', () => {
  const d = deliveryOf(capture({ forward_status: 200, forward_error: 'timeout' }))
  assert.equal(d.kind, 'delivered')
})

test('formatMs is readable at both scales', () => {
  assert.equal(formatMs(42), '42ms')
  assert.equal(formatMs(999), '999ms')
  assert.equal(formatMs(1500), '1.5s')
  assert.equal(formatMs(undefined), '')
})
