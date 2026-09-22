/**
 * Opening the CLI's inbox in the browser, from the link `hooklens forward`
 * prints: `http://localhost:8080/#slug=<slug>&token=<token>`.
 *
 * The FRAGMENT, not a query string, because the fragment is never sent to any
 * server -- not in the request line, not in the Referer header -- so the token
 * reaches this page's JavaScript and nothing else. A query string would put it
 * in access logs and proxy logs, the leak unit 08 refused.
 * See docs/learn/40-dead-inboxes-and-handoff-links.md.
 *
 * No React here: this is the decision logic, testable with node --test.
 */

import type { Inbox } from './api'

// Mirrors server.validSlug: 3-32 of [a-z0-9-], no leading or trailing hyphen.
// Validated here too so a malformed link is ignored rather than stored and
// then rejected by the server as a mystery "invalid token".
const SLUG = /^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$/

/** The inbox a link's fragment carries, or null if it carries none. */
export function inboxFromHash(hash: string): Inbox | null {
  const h = hash.startsWith('#') ? hash.slice(1) : hash
  if (!h) return null
  const p = new URLSearchParams(h)
  const slug = p.get('slug')
  const token = p.get('token')
  if (!slug || !token || !SLUG.test(slug)) return null
  return { slug, token }
}

/** Whether the address bar holds a token that should be removed from it. */
export function hashHasToken(hash: string): boolean {
  return new URLSearchParams(hash.startsWith('#') ? hash.slice(1) : hash).has('token')
}

/**
 * What to do with a linked inbox, given the one this browser already holds.
 *
 * - `adopt`: nothing stored, or the same inbox -- no loss either way (for the
 *   same slug the link's token is the one the CLI is actually using).
 * - `ask`: a DIFFERENT inbox is stored. The browser remembers one inbox and a
 *   token cannot be recovered from the server -- it is stored there only as a
 *   hash -- so silently switching would permanently lose access to the
 *   current one. That decision belongs to the person, not to a link.
 * - `none`: the link carried no inbox.
 */
export function handoffDecision(stored: Inbox | null, linked: Inbox | null): 'adopt' | 'ask' | 'none' {
  if (!linked) return 'none'
  if (!stored || stored.slug === linked.slug) return 'adopt'
  return 'ask'
}
