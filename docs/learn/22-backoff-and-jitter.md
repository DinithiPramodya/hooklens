# 22 — Exponential backoff, jitter, and the thundering herd

*Phase 3, unit 6. Covers the curriculum bullet "exponential backoff, jitter, and the
thundering-herd problem". This is failure mode 6 from `PLAN.md`: the network flaps, and the
CLI comes back on the same URL without being restarted.*

## Brief

**What is this thing?** A reconnection policy: after a connection drops, how long to wait
before trying again, and how that wait changes as attempts keep failing.

**What problem does it exist to solve?** The naive answer is to retry immediately, which is
a tight loop against a server that has just demonstrated it cannot serve you. It burns CPU
and battery, floods logs, and adds load to something already struggling.

The obvious fix is a fixed delay — try every five seconds. Better, and it contains a
worse failure. Picture a server with ten thousand tunnels attached that restarts. Every
client notices within a few milliseconds of the same instant, every client waits exactly
five seconds, and ten thousand reconnections arrive together. The server, still warming up,
falls over. Every client waits five seconds. This is the **thundering herd**, and its
signature is that the retry storm becomes the outage — the original fault may have lasted
two seconds.

**How does it actually work underneath?** Two mechanisms, and they solve different halves.

**Exponential backoff** handles rate: wait `base × 2^attempts`, capped. One failure waits
half a second, ten consecutive failures wait the cap. A client that has been failing for an
hour is not asking sixty times a minute. This converts hammering into occasional checking.

It does nothing about synchronisation. Clients that failed together still back off
together, so they still arrive together — the waves simply get further apart.

**Jitter** handles that: randomise the delay. The usual form, and the one with the best
published simulation results, is **full jitter** — sleep for a uniformly random duration
between zero and the current exponential ceiling, rather than for the ceiling itself.

One level down on why randomness helps at all, because "it spreads them out" is the
description and not the reason. The problem was never the average request rate; ten
thousand clients retrying once every five seconds is two thousand per second, which is
nothing. The problem is that identical timers turn those clients into a **periodic
impulse**: all the load in a few milliseconds, none for the rest. Queues fail on peaks, not
averages. Jitter converts the impulse train into a roughly uniform arrival process with the
same mean and a vastly lower peak.

**What are the sharp edges?**

- **Resetting the counter on connection is wrong.** A server that accepts and immediately
  drops gives you a tight loop in which every attempt "succeeded". Reset only after a
  session that lasted long enough to count.
- **Not everything should be retried.** A rejected token will never start working.
  Retrying it forever is a busy-wait that also looks like a credential attack.
- **Cap the delay**, or doubling reaches hours and the tool looks dead.
- **Sleep cancellably**, or ctrl-c takes thirty seconds.
- **Jitter needs real randomness per process.** Clients seeded identically compute
  identical "random" delays and stay synchronised.

**In hooklens:** the CLI reconnects with full jitter, base 500ms capped at 30s, resetting
only after a session that lasted at least 30 seconds, and re-registers the **same slug**
from the config file — so the public URL survives a flap. That last part is the feature
ngrok charges for.

## Decisions

**Full jitter, not exponential-only and not "exponential plus a little noise".**
Sleep for a uniformly random duration in `[0, ceiling)` rather than for the ceiling. The
alternatives and why they lose: **no jitter** keeps clients that failed together arriving
together, so the waves merely get further apart; **equal jitter** (`ceiling/2 + rand(0,
ceiling/2)`) still concentrates arrivals in the back half of the window. Full jitter
spreads them across the whole window and has the best published simulation results for
both server load and total completion time.
*Wrong call if* a client needed a predictable worst-case retry time — full jitter can draw
a very short delay twice in a row. Nothing here needs that guarantee.

**The attempt counter resets only after a session that lasted 30 seconds.**
Resetting on connect is the obvious choice and it is a bug: a server that accepts and
immediately drops produces a tight reconnect loop in which **every attempt succeeded**, so
the backoff never grows and the herd never disperses. Requiring the session to have lasted
means only a genuinely working connection clears the counter. `TestClientReconnectsRepeatedly`
is the test that would catch the mistake.

**Some failures are permanent, and `CloseError.Permanent()` decides which.**
`unauthorized` and `unsupported_version` describe the *client* being wrong, and no amount
of waiting changes that — retrying them forever is a busy-wait that also looks like a
credential attack from the server's side. Everything else defaults to retryable, because
being wrong in that direction merely costs a few seconds while being wrong in the other
gives up on a server that was restarting.
This is why the close **code** was a stable string from unit 18 rather than a message: the
CLI switches on it, and a decision that depended on wording would break the first time
someone improved a sentence.

**A typed `*CloseError` rather than a formatted string.**
The caller has a decision to make on it, and `strings.Contains(err.Error(), "unauthorized")`
is how that decision silently stops working. `Reason` is still carried verbatim so the CLI
can print the server's own sentence.

**`base << attempt`, clamped, rather than `math.Pow`.**
No floats, no conversion, and the `min(attempt, 32)` plus a `<= 0` check means a shift
cannot overflow into a negative duration — which would silently become a zero-length sleep,
i.e. a hot loop. The guard is unreachable with today's constants and is there because
changing one constant should not reintroduce a spin.

**Base 500ms, cap 30s.**
The overwhelmingly common cause of a drop is a laptop's Wi-Fi blinking, and making that
cost five seconds of downtime would be felt on every coffee-shop connection. The cap keeps
a long outage from reaching delays where the tool looks dead rather than patient.

**`math/rand/v2`'s global source, not a seeded one.**
It is automatically and randomly seeded per process. That matters here specifically:
clients seeded identically compute identical "random" delays and stay in lockstep, which
is the precise failure jitter exists to prevent. Not a place to be clever about
determinism.

**A cancellable sleep.**
`time.Sleep` would make ctrl-c take up to 30 seconds, which reads as a hung program. A
timer plus `select` on `ctx.Done()`.

**`OnDisconnect` prints, it does not log.**
A tunnel that is down while the terminal looks normal is how webhooks get silently missed.
The one thing the user must see goes to stdout; the full error stays in the log at `-v`.

**`tunnel.ShortError` for the printed reason.**
Go's dial failures are four nested clauses and only the last is actionable. The first
version of this printed the raw error and produced 200-character lines that nobody would
read — visible in the demo output below, and fixed before committing. The same helper
already existed for the local-app case; exporting it was the whole fix.

**ASCII only in CLI output.**
An em-dash rendered as `â€"` in the Windows console. Found by running the thing rather
than by reading the code, and it was in the line a user reads *when something has gone
wrong* — the worst possible place for a glyph that says "this program is broken".

## Walkthrough

### `internal/tunnel/backoff.go`

The three constants each carry the reasoning for their value, and `stableSession` (`:30`)
is the one that is a correctness property rather than a tuning knob.

`next` (`:48`) — the shift, the clamp, the overflow guard, and `rand.N(ceiling)` for full
jitter. The comment on `rand/v2` (`:63`) explains why the seeding behaviour is load-bearing.

`sleep` (`:73`) returns false on cancellation, which is what lets `Run` distinguish "the
wait finished" from "we are shutting down".

### `internal/tunnel/protocol.go`

`CloseError` (`:147`) and `Permanent()` (`:168`). The `default: return false` arm is the
decision: unknown codes are retried.

### `internal/tunnel/client.go`

`Run` (`:124`) is the loop. Its shape is worth reading in order: connect, measure the
session, check cancellation *before* treating anything as a failure, reset the counter only
if the session lasted, return on a permanent error, otherwise sleep and go again.

`connectOnce` (`:163`) is what `Run` used to be — unchanged behaviour, one connection.

The `TypeClose` arms in `handshake` (`:224`) and `serve` (`:275`) now build
`*CloseError` instead of a formatted string, which is what makes the permanence check
possible upstream.

### `cmd/hooklens/forward.go`

`connected` (`:194`) so a reconnect prints one line instead of repeating the banner every
time the Wi-Fi blinks.

`OnDisconnect` (`:213`) — `ShortError`, ASCII, and the comment recording both reasons.

## Verified

**Eight unit tests** for the timing, and they assert properties rather than values, because
full jitter means no individual delay can be predicted:

- `TestBackoffCeilingGrowsAndCaps` takes the **maximum over 3000 draws** at each attempt.
  That maximum must be below the ceiling (`rand.N` is exclusive) and close to it, and the
  ceiling must double and then stop at the cap. Attempts 20 through 1000 are checked
  separately for a negative delay — the shift-overflow hot loop.
- `TestBackoffIsJittered` asserts 200 draws produce at least 100 distinct values. Without
  jitter it produces exactly one, and clients reconnect in lockstep.
- `TestBackoffSpreadsLoad` is the thundering-herd property stated directly: a thousand
  clients drop together, and no one-tenth slice of the window may hold anything close to
  all of them.
- `TestSleepIsCancellable`, and `TestCloseErrorPermanence` listing every code on both sides
  of the line.

**Five integration tests** with a real client against a real server:
reconnect after a silent drop *and forward successfully afterwards*; reconnect three times
in a row; **stop** on a rejected token; stop on ctrl-c during a backoff sleep; and survive a
server that goes away entirely and comes back.

**Then the live version.** Server, local app and CLI running; the server killed out from
under the CLI; the server brought back:

```
  disconnected: No connection could be made because the target machine actively refused it. - retrying in 900ms
  disconnected: No connection could be made because the target machine actively refused it. - retrying in 1s
  disconnected: No connection could be made because the target machine actively refused it. - retrying in 2.1s
  disconnected: No connection could be made because the target machine actively refused it. - retrying in 5.8s
  reconnected - same URL
```

Growing, jittered, and it came back on its own. A webhook sent afterwards reached the local
app and returned its 201 with both cookies — a reconnect that registered but could not
forward would be worse than none, so that check matters.

### The test-isolation bug this unit created

The full suite failed on `TestShutdownClosesTunnels` — a test from unit 18 that this unit
did not touch — while passing when run alone and passing under `-race` in WSL. Three
properties that together say "ordering, not logic".

The cause: **every client test used the same slug**, so a client still reconnecting after
its test finished would register for the same inbox and **evict** the next test's
connection. Failure mode 5 working exactly as designed, aimed at the test suite. Worth
noticing that the feature under test was the thing that broke the tests — reconnection is
what made leftover clients keep coming back.

Fixed properly rather than with a sleep: each test gets its own slug and endpoint id via
`uniqueInbox`, and `startClient`'s cleanup now **cancels and waits** for `Run` to return
rather than cancelling and hoping.

That second fix needed a second channel. The cleanup originally waited on the same `done`
channel the test reads from, so any test that checked `Run`'s return value consumed the
only value and left the cleanup blocked until its own timeout. Closing a channel is a
broadcast; sending on one is not.

Three consecutive full-package runs clean afterwards, plus `-race`.
