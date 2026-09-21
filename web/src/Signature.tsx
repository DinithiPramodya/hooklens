import { useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import { verifyRequest, type Inbox, type VerifyResult } from './lib/api'

/**
 * The signature panel.
 *
 * Its job is not to say VALID or INVALID -- that part is one boolean. Its
 * job is to show the canonical string, because nearly every real signature
 * bug is a canonical-string bug and none of them is diagnosable from a
 * boolean. See docs/learn/26-hmac.md.
 */
export function Signature({ id, inbox }: { id: string; inbox: Inbox }) {
  // useState, and deliberately nowhere else.
  //
  // A signing secret is a bearer credential for FORGING events: anyone
  // holding it can send the developer's own application a payload it will
  // believe. localStorage persists across restarts and users of a shared
  // machine and is readable by any injected script; sessionStorage survives
  // reloads. Component state lives in memory for as long as the tab does and
  // is gone on refresh -- mildly annoying, and the right trade here.
  const [secret, setSecret] = useState('')

  // A mutation, not a query. Verification is an ACTION: it has no cache key,
  // must never run on mount, and runs when a person asks.
  // See docs/learn/29-mutations-and-secrets.md.
  const verify = useMutation({
    mutationFn: (s: string) => verifyRequest(id, inbox.token, s),
  })

  return (
    <section className="sig">
      <h3>signature</h3>

      <form
        className="row"
        onSubmit={(e) => {
          e.preventDefault()
          if (secret) verify.mutate(secret)
        }}
      >
        <input
          type="password"
          // autoComplete="off" is widely ignored by password managers.
          // "new-password" is the form they actually respect -- and it is
          // still a request, not a guarantee.
          autoComplete="new-password"
          placeholder="signing secret (whsec_…)"
          value={secret}
          onChange={(e) => setSecret(e.target.value)}
          className="secret"
        />
        {/* Disabled while pending: a double-click would send two requests,
            and the button must not imply a second check is happening. */}
        <button type="submit" disabled={!secret || verify.isPending}>
          {verify.isPending ? 'checking…' : 'verify'}
        </button>
      </form>

      <p className="muted tiny">
        Used for this check and never stored — not on the server, not in this browser.
        It is gone when you reload.
      </p>

      {verify.isError && (
        <p className="err">
          {verify.error instanceof Error ? verify.error.message : 'verification failed'}
        </p>
      )}

      {verify.data && <VerifyPanel result={verify.data} />}
    </section>
  )
}

function VerifyPanel({ result: r }: { result: VerifyResult }) {
  if (!r.detected) {
    return (
      <div className="delivery">
        <p>no signature header recognised</p>
        {r.hint && <p className="muted">{r.hint}</p>}
      </div>
    )
  }

  return (
    <div className={r.valid ? 'delivery ok' : 'delivery fail'}>
      <p>
        <strong>{r.valid ? 'VALID' : 'INVALID'}</strong>
        <span className="muted"> · {r.provider}</span>
        {r.age_seconds !== undefined && (
          <span className="muted"> · signed {r.age_seconds}s before it arrived</span>
        )}
      </p>
      {r.problem && <p className="muted">{r.problem}</p>}
      {r.hint && <p className="muted">{r.hint}</p>}

      {/* The reason this panel exists. Shown whether the check passed or
          failed -- a developer whose own code signs the wrong thing needs
          something to compare against. */}
      {r.signed_string !== undefined && (
        <details open={!r.valid}>
          <summary>the exact bytes that were signed</summary>
          <pre className="raw sigstring">{r.signed_string}</pre>
          <table className="headers">
            <tbody>
              <tr>
                <td className="hname">expected</td>
                <td className="hval">{r.expected}</td>
              </tr>
              <tr>
                <td className="hname">provided</td>
                <td className="hval">{r.provided}</td>
              </tr>
            </tbody>
          </table>
        </details>
      )}
    </div>
  )
}
