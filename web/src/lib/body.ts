/**
 * Turning a captured body back into something renderable.
 *
 * The server stores bodies as raw bytes and hands them over base64-encoded,
 * because a JSON string must be valid UTF-8 and a webhook body need not be.
 * Everything here is pure -- bytes in, a decision out -- which is what makes
 * it testable without a browser.
 */

export type BodyView =
  | { kind: 'empty' }
  | { kind: 'json'; value: unknown; text: string }
  | { kind: 'text'; text: string }
  | { kind: 'binary'; bytes: Uint8Array }

/** Decode base64 to bytes. */
export function decodeBase64(b64: string): Uint8Array {
  const bin = atob(b64)
  const out = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
  return out
}

/**
 * Decide how to present a body.
 *
 * The order matters and encodes what an inspector is for. Binary is detected
 * FIRST, because a body containing NUL or invalid UTF-8 is not text and
 * rendering it as text produces replacement characters that look like data
 * corruption the sender did not cause. Only then do we try JSON, and text is
 * the fallback -- notably including MALFORMED JSON, which is very often the
 * exact thing being debugged and must never be swallowed into an error state.
 */
export function classifyBody(bytes: Uint8Array): BodyView {
  if (bytes.length === 0) return { kind: 'empty' }

  if (!isProbablyText(bytes)) return { kind: 'binary', bytes }

  // fatal: true makes the decoder throw on invalid UTF-8 rather than silently
  // substituting U+FFFD. Silent substitution is the failure mode we are trying
  // to avoid: it turns "this body is not text" into "this body contains
  // strange characters", which sends the user hunting for a bug in the sender.
  let text: string
  try {
    text = new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch {
    return { kind: 'binary', bytes }
  }

  try {
    return { kind: 'json', value: JSON.parse(text) as unknown, text }
  } catch {
    return { kind: 'text', text }
  }
}

/**
 * A cheap heuristic: a NUL byte means this is not text.
 *
 * Deliberately not a content-type check. Providers mislabel constantly -- and
 * a tool whose job is showing what really arrived should trust the bytes over
 * the sender's claim about them.
 */
function isProbablyText(bytes: Uint8Array): boolean {
  const limit = Math.min(bytes.length, 512)
  for (let i = 0; i < limit; i++) {
    if (bytes[i] === 0) return false
  }
  return true
}

/** A hex + ASCII dump, the way any other byte viewer shows binary. */
export function hexDump(bytes: Uint8Array, maxBytes = 2048): string {
  const n = Math.min(bytes.length, maxBytes)
  const lines: string[] = []

  for (let off = 0; off < n; off += 16) {
    const row = bytes.subarray(off, Math.min(off + 16, n))
    const hex = Array.from(row, (b) => b.toString(16).padStart(2, '0'))
    // A gap after eight bytes, so the eye can count without counting.
    const hexPart = [hex.slice(0, 8).join(' '), hex.slice(8).join(' ')].join('  ').padEnd(49)
    const ascii = Array.from(row, (b) => (b >= 0x20 && b < 0x7f ? String.fromCharCode(b) : '.')).join('')
    lines.push(`${off.toString(16).padStart(8, '0')}  ${hexPart} |${ascii}|`)
  }

  if (bytes.length > n) {
    lines.push(`… ${bytes.length - n} more bytes`)
  }
  return lines.join('\n')
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(1)} MB`
}
