import { test } from 'node:test'
import assert from 'node:assert/strict'

import { handoffDecision, hashHasToken, inboxFromHash } from './handoff.ts'
import { isPermanentStatus } from './sse.ts'

const slug = 'xhaemjkqgw64bxg4wvmlan53oe'
const token = 'uMLd4XO-OwUJ-hKZUl2wodG1l9JhvEXcVMAj0CpqljA'

test('inboxFromHash reads the link the CLI prints', () => {
  assert.deepEqual(inboxFromHash(`#slug=${slug}&token=${token}`), { slug, token })
})

test('inboxFromHash ignores a missing or partial fragment', () => {
  assert.equal(inboxFromHash(''), null)
  assert.equal(inboxFromHash('#'), null)
  assert.equal(inboxFromHash(`#slug=${slug}`), null)
  assert.equal(inboxFromHash(`#token=${token}`), null)
})

test('inboxFromHash rejects a slug the server would never route', () => {
  // Same rule as server.validSlug. Storing these would only produce a
  // confusing "invalid token" from the server a moment later.
  for (const bad of ['AB', 'a', '-abc', 'abc-', 'has space', 'UPPER', 'a'.repeat(33)]) {
    assert.equal(inboxFromHash(`#slug=${encodeURIComponent(bad)}&token=${token}`), null, bad)
  }
})

test('hashHasToken spots a token to strip, even in a malformed link', () => {
  assert.equal(hashHasToken(`#slug=${slug}&token=${token}`), true)
  assert.equal(hashHasToken(`#slug=AB&token=${token}`), true)
  assert.equal(hashHasToken('#something-else'), false)
  assert.equal(hashHasToken(''), false)
})

test('handoffDecision adopts when nothing would be lost', () => {
  const linked = { slug, token }
  assert.equal(handoffDecision(null, linked), 'adopt')
  assert.equal(handoffDecision({ slug, token: 'older' }, linked), 'adopt')
})

test('handoffDecision asks before replacing a different inbox', () => {
  // The stored inbox's token cannot be recovered from the server, so a link
  // must never silently cost the user access to it.
  assert.equal(handoffDecision({ slug: 'another-inbox', token: 't' }, { slug, token }), 'ask')
})

test('handoffDecision does nothing without a link', () => {
  assert.equal(handoffDecision({ slug, token }, null), 'none')
  assert.equal(handoffDecision(null, null), 'none')
})

test('only a rejected credential stops the live stream', () => {
  assert.equal(isPermanentStatus(401), true)
  assert.equal(isPermanentStatus(403), true)
  // These may heal. Treating them as permanent would log every tab out on a
  // server restart.
  for (const s of [0, 404, 429, 500, 502, 503, 504]) assert.equal(isPermanentStatus(s), false, String(s))
})

test('inboxFromHash decodes exactly what the CLI encodes', () => {
  // cmd/hooklens/forward.go builds the fragment with url.Values.Encode, which
  // percent-encodes "=", "+" and "/". location.hash hands the page that RAW
  // form, and URLSearchParams must decode it back byte for byte -- a "+" read
  // as a space would be a token that silently never matches.
  const encoded = '#slug=abc123def&token=tok_-x%3Dy%2Bz%2F'
  assert.deepEqual(inboxFromHash(encoded), { slug: 'abc123def', token: 'tok_-x=y+z/' })
})
