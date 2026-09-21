import { test } from 'node:test'
import assert from 'node:assert/strict'
import { classifyBody, decodeBase64, hexDump, formatBytes } from './body.ts'

const utf8 = (s: string) => new TextEncoder().encode(s)

test('an empty body is empty, not text', () => {
  assert.equal(classifyBody(new Uint8Array()).kind, 'empty')
})

test('valid JSON is parsed', () => {
  const v = classifyBody(utf8('{"a":1}'))
  assert.equal(v.kind, 'json')
  if (v.kind === 'json') assert.deepEqual(v.value, { a: 1 })
})

test('MALFORMED json falls back to text, never to an error', () => {
  // The case that matters most: broken JSON from a provider is very often the
  // exact thing being debugged. Swallowing it into an error state would hide
  // the answer the user came for.
  const v = classifyBody(utf8('{"a":1'))
  assert.equal(v.kind, 'text')
  if (v.kind === 'text') assert.equal(v.text, '{"a":1')
})

test('a NUL byte means binary, whatever else is in there', () => {
  const v = classifyBody(new Uint8Array([0x7b, 0x00, 0x7d]))
  assert.equal(v.kind, 'binary')
})

test('invalid UTF-8 is binary, not text with replacement characters', () => {
  // 0xff is never valid UTF-8. Decoding non-fatally would yield U+FFFD and
  // make a binary body look like corrupted text the sender was to blame for.
  const v = classifyBody(new Uint8Array([0xff, 0xfe, 0x41]))
  assert.equal(v.kind, 'binary')
})

test('gzip magic is binary', () => {
  assert.equal(classifyBody(new Uint8Array([0x1f, 0x8b, 0x08, 0x00])).kind, 'binary')
})

test('plain text stays text', () => {
  const v = classifyBody(utf8('hello=world&x=1'))
  assert.equal(v.kind, 'text')
})

test('JSON scalars are JSON, not text', () => {
  // "null", "true" and "42" are all valid JSON documents. Treating them as
  // text would be defensible but inconsistent; treating them as JSON keeps
  // the type visible in the tree.
  for (const s of ['null', 'true', '42', '"a string"']) {
    assert.equal(classifyBody(utf8(s)).kind, 'json', s)
  }
})

test('decodeBase64 round-trips arbitrary bytes', () => {
  const bytes = new Uint8Array([0, 255, 254, 31, 139, 8, 0, 65])
  const b64 = btoa(String.fromCharCode(...bytes))
  assert.deepEqual(decodeBase64(b64), bytes)
})

test('hexDump renders offset, hex and ascii', () => {
  const out = hexDump(utf8('hi'))
  assert.match(out, /^00000000 {2}68 69/)
  assert.match(out, /\|hi\|$/)
})

test('hexDump caps output and says how much it dropped', () => {
  const out = hexDump(new Uint8Array(100), 32)
  assert.match(out, /68 more bytes/)
})

test('formatBytes', () => {
  assert.equal(formatBytes(0), '0 B')
  assert.equal(formatBytes(1023), '1023 B')
  assert.equal(formatBytes(2048), '2.0 KB')
  assert.equal(formatBytes(3 * 1024 * 1024), '3.0 MB')
})
