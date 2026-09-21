# 17 — NAT and firewalls: why the internet cannot reach your laptop

*Phase 3, unit 1. Covers the curriculum bullets "NAT and firewalls: why the internet cannot
open a connection to your laptop, and why your laptop can open one outward" and "connection
tracking". This is the foundation the whole tunnel rests on — every design choice in Phase 3
is downstream of the asymmetry described here.*

## Brief

**What is this thing?** NAT — Network Address Translation — is your router rewriting the
addresses on packets as they pass through it, so that many devices can share one public IP
address. Your laptop has a private address like `192.168.1.47`. That address is not
reachable from the internet, and it is not even unique: millions of laptops have the same
one. Your home or office has exactly one public address, and the router sits between.

**What problem does it exist to solve?** IPv4 has about 4.3 billion addresses and the world
has far more connected devices than that. NAT, from the mid-1990s, let one public address
serve a whole household. It was explicitly a stopgap until IPv6 arrived. IPv6 did arrive,
and NAT is still everywhere — it became permanent infrastructure, and it quietly changed
what kind of programs you can write.

**How does it actually work underneath?** The mechanism is a table, and everything follows
from how rows get into it.

When your laptop opens a connection outward, the router picks a spare port on its public
address and writes a row: *this private address and port, on this outbound connection to
that destination, is now that public port*. Outbound packets get their source rewritten to
the public pair. Reply packets that match the row get rewritten back and delivered inward.
This is **connection tracking**: the router is not a dumb relay, it holds state per
connection.

The asymmetry is the entire point, and it is worth stating precisely. **Rows are created by
outbound packets.** An inbound packet that matches no existing row is not refused as a
matter of policy — the router genuinely has no way to know which of the twenty devices
behind it the packet is for. There is no information. So it is dropped. A firewall may add
deliberate policy on top, but NAT alone already makes unsolicited inbound connections
impossible.

The other half: rows **expire**. A connection that goes quiet has its row reclaimed, and
once it is gone, replies on that connection have nowhere to go.

**What are the sharp edges?**

- *"Just forward a port"* requires access to the router, a stable public address, and does
  nothing under **CGNAT** — carrier-grade NAT, where your ISP NATs you too. Under CGNAT you
  have no public address to forward from. This is normal on mobile networks and increasingly
  common on home broadband.
- Idle timeouts are **not advertised and vary wildly**, from about 30 seconds on some mobile
  carriers to hours. You cannot query them. You must assume the worst and keep the
  connection alive yourself.
- The direction of a connection is fixed when it is established — but **data flows both ways
  afterwards**. That sentence is the whole trick that makes tunnels possible.

**In hooklens:** Stripe cannot dial your laptop, and no amount of code on our side changes
that. So the CLI dials *out* to the hooklens server and holds that connection open. The
server now has a live path to your machine that it never had to knock for, created by you.
When a webhook arrives, it travels down that already-open line. Every tunnel — ngrok,
Cloudflare Tunnel, Tailscale, VS Code port forwarding — has exactly this shape, for exactly
this reason.

Two Phase 3 failure modes are already visible in the paragraphs above. Row expiry is
failure mode 7, which is why we will send an application-level ping every 20 seconds. And
"the connection is fixed at setup but carries data both ways" is why the server can push a
`request` frame down a link the client opened.

## Observed on the development machine

Not taken on faith — read off the actual adapter, 2026-09-21. Addresses partly redacted
because this repository is public.

```
adapter : Wi-Fi
  ipv4  : 192.168.1.7        <-- private, behind NAT
  gw    : 192.168.1.1        <-- the NAT box
  ipv6  : 2402:d000:…        <-- globally routable
```

Both halves of the story are present at once, and the second is a **refinement of the
brief above**.

On IPv4 this is the textbook case: a private address, a router holding the translation
table, nothing outside able to address the machine.

On IPv6 the machine has a *globally routable* address. `2402:d000::/32` is public space —
in principle reachable from anywhere. No NAT, because IPv6 has enough addresses that NAT
was never needed.

So the accurate claim is: **NAT is not the only thing preventing inbound connections, it is
the one that is unavoidable.** On IPv6 the blocker is a stateful firewall instead — the
router and the host both drop unsolicited inbound by default, tracking connections exactly
as NAT does but without rewriting addresses. Different mechanism, same outcome, and the
same consequence for us: a provider cannot dial in.

The connection table showed the asymmetry directly. Every established connection was
**outbound, to port 443**, dozens of them — each a row created by this machine dialling
out. Not one was created by something dialling in.

That is the whole argument for the tunnel, and it is why no amount of server-side code
could have avoided it.
