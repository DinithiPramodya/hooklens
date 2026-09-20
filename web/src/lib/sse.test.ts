import { test } from 'node:test'
import assert from 'node:assert/strict'
import { SSEParser } from './sse.ts'

/**
 * Run with `npm test`, which is `node --test`. No test runner dependency:
 * Node 24 strips TypeScript types natively and ships a test runner, so this
 * costs nothing to keep.
 *
 * Only the parser is tested here. It is pure text-in/events-out, which makes
 * it both the riskiest part of the client and the only part testable without a
 * browser -- the transport needs fetch, streams and a server, and is covered
 * end to end on the Go side instead.
 */

test('parses a single event', () => {
  const p = new SSEParser()
  assert.deepEqual(p.push('event: capture\ndata: {"id":1}\n\n'), [
    { event: 'capture', data: '{"id":1}', id: undefined },
  ])
})

test('reassembles an event split across chunk boundaries', () => {
  // The case that matters. A chunk boundary can fall anywhere, including
  // mid-field-name. A parser that assumed one chunk is one event would work
  // perfectly in development and corrupt events under load.
  const p = new SSEParser()
  assert.deepEqual(p.push('event: cap'), [])
  assert.deepEqual(p.push('ture\ndata: {"i'), [])
  assert.deepEqual(p.push('d":1}\n\n'), [
    { event: 'capture', data: '{"id":1}', id: undefined },
  ])
})

test('emits several events arriving in one chunk', () => {
  const p = new SSEParser()
  assert.deepEqual(
    p.push('data: a\n\ndata: b\n\n').map((e) => e.data),
    ['a', 'b'],
  )
})

test('a comment frame yields no event', () => {
  // Our heartbeats are exactly this: bytes on the wire that keep the
  // connection alive and must not reach the application.
  const p = new SSEParser()
  assert.deepEqual(p.push(': keepalive\n\n'), [])
})

test('a heartbeat between events corrupts neither', () => {
  const p = new SSEParser()
  p.push('data: start\n')
  assert.deepEqual(
    p.push('\n: keepalive\n\ndata: next\n\n').map((e) => e.data),
    ['start', 'next'],
  )
})

test('rejoins multi-line data with newlines', () => {
  // The inverse of what the server does when splitting a payload containing
  // newlines across several data: lines.
  const p = new SSEParser()
  assert.equal(p.push('data: first\ndata: second\n\n')[0].data, 'first\nsecond')
})

test('defaults the event name to "message"', () => {
  const p = new SSEParser()
  assert.equal(p.push('data: x\n\n')[0].event, 'message')
})

test('treats the space after the colon as format, not value', () => {
  const p = new SSEParser()
  assert.equal(p.push('data:no-space\n\n')[0].data, 'no-space')
  assert.equal(p.push('data:  leading\n\n')[0].data, ' leading')
})

test('captures the id field', () => {
  const p = new SSEParser()
  assert.equal(p.push('id: 42\ndata: x\n\n')[0].id, '42')
})
