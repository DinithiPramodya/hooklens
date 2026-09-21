# 30 — Three flaky tests, and what each one actually was

*Phase 4, unit 5. Not a feature. A full-suite run started failing intermittently on tests
nothing had touched, and the three causes turned out to be different from each other and
different from what the symptom suggested. Worth its own note because diagnosing
intermittent failures is a skill, and because two of the three were real bugs rather than
test bugs.*

## Brief

**What is this thing?** An intermittent test failure — one that passes alone, passes most
of the time, and fails under load or in a particular order.

**What problem does it exist to solve?** Nothing; it is a problem. But the *shape* of an
intermittent failure carries information that a deterministic one does not, and reading
that shape is most of the diagnosis.

**How does it actually work underneath?** The useful question is never "why did this
fail?" but "what does the *pattern* of failure rule out?"

- **Passes alone, fails in a suite** → shared state or ordering, not logic. Logic does not
  care what ran before it.
- **Passes at `-count=1`, fails at `-count=3`** → state that outlives a test within one
  process: a goroutine, a registry entry, a port.
- **Fails under parallel load only** → a race whose window is normally too small, or a
  resource limit.
- **Hangs rather than fails** → something is waiting on something that will never happen.
  A wrong answer arrives fast; a missing wakeup never arrives at all.
- **Fails with a *different* error than expected** → the most informative case, because it
  names which layer got as far as running. `ErrTimeout` where `ErrNoTunnel` was expected
  meant the registry was *right* and the problem was downstream of it.

**What are the sharp edges?**

- **A retry loop hides the cause.** Re-running until it passes converts a diagnosable bug
  into an occasional mystery.
- **A `sleep` fixes the symptom and keeps the race**, now with a longer window before it
  reappears on slower hardware.
- **A test that silently skips its own precondition** turns a race into a timeout with a
  misleading message — which is worse than a crash.
- **The fix belongs at the layer that owns the invariant**, not at the assertion.

## Decisions

**Three failures, three different causes, none of them the one the message named.**

### 1. `TestClientSurvivesServerRestart` — a precondition that skipped itself

Reported as *"no reconnection after the server came back within 20s"*, which reads like a
reconnection bug. It was not.

`waitConnect` returns when the **client** reads `hello_ok`. The server writes that frame
*before* it registers the client in the hub. So the test's next line —
`ts.hub.clients[endpointID]` — could legitimately find `nil`, and the code said:

```go
if victim != nil {
    victim.conn.CloseNow()
}
```

No drop happened, so nothing reconnected, so the test waited twenty seconds and blamed
reconnection. The `if victim != nil` is the whole bug: it converted "my precondition did
not hold" into "the feature is broken".

Fixed by waiting for the *server-side* registration with `waitConnected`, and by making a
nil client `t.Fatal` instead of a silent skip. **A test that cannot set up its own
scenario must say so loudly**, because the alternative is a failure message pointing at
innocent code.

### 2. The same test — a deadlock in the teardown

`defer srv.Close()` with the client still attached. `httptest.Server.Close` waits for
outstanding requests, and a tunnel handler does not return while a client is connected.
Defers run LIFO and `t.Cleanup` runs after all of them, so the client's own stopper was
always too late.

That is what turned a 20-second failure into an 11-minute hang. Fixed by stopping the
client explicitly before closing the server — which meant `startClient` had to return its
stopper as well as registering it as a cleanup.

### 3. `TestShutdownClosesTunnels` — an RST discarding the payload

A **product** bug, not a test bug, and the only one of the three that would have affected
a user.

`closeWith` wrote the application-level close frame and then called `CloseNow`. `CloseNow`
aborts the connection rather than performing a closing handshake — and an abort can send a
TCP **RST**, which makes the peer **discard data already sitting in its receive buffer**.
Including the close frame just written to explain the disconnection.

So on a server shutdown the CLI would sometimes print an unexplained EOF instead of
"server is shutting down" — the exact failure the application-level frame exists to
prevent, reintroduced by the very call meant to make the teardown quick.

This was unit 19's fix over-applied. That unit changed `Close` to `CloseNow` to avoid
waiting on a closing handshake with a peer that had gone away, which was right; the part
that was wrong was assuming the wait is the only difference. `closeWith` now attempts a
graceful close **bounded by one second**, falling back to `CloseNow`. A live peer answers
in microseconds; a dead one costs a second and no more, and the eviction path already runs
this in a goroutine.

## Verified

Honestly, and including what is *not* settled.

- `TestClientSurvivesServerRestart`: **10 consecutive passes** after the fix, where it had
  been failing roughly two runs in three.
- The shutdown path: a purpose-built loop running the cancel-and-read assertion **30 times
  in one process** reports 0 failures. Isolated, 20/20.
- Full suite: **4 consecutive clean runs**, plus `-race`.

What I could not do is reproduce the `TestShutdownClosesTunnels` failure deterministically.
It appeared twice — once under `-count=3` and once in a `./...` run with packages building
and testing in parallel — and not once in any targeted loop. The RST explanation fits the
evidence and the fix is correct on its own terms, but I have not *proven* it was the cause,
and saying otherwise would be dressing a plausible story up as a confirmed one.

If it recurs, the next thing to check is file descriptors and ephemeral ports: this suite
stands up a great many `httptest` servers and WebSocket connections in one process, and
resource exhaustion under parallel load produces exactly this signature.
