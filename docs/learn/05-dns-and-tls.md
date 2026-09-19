# 05 — DNS, and the certificates that ride on it

*Phase 0, the deferred unit. Covers the curriculum bullets "DNS: A records, CNAMEs, wildcard
records, what resolution actually does" and "TLS: the handshake, what a certificate proves,
what a CA is, why wildcard certs need DNS-01 rather than HTTP-01".*

*Written after the rest of Phase 0 rather than in build order: it was originally scheduled
second, when the plan assumed a purchased domain. There is no domain and no code in this
unit — everything below is verified against real public hosts instead.*

## Brief — DNS

**What is it?** A distributed database that turns a name into an address. The interesting
word is *distributed*: no single machine holds the answers.

**What problem does it solve?** Humans cannot remember `140.82.121.4`, and more importantly
addresses change. Before DNS there was one file — `HOSTS.TXT`, maintained by hand at
Stanford, downloaded periodically by every machine on ARPANET. That worked for a few hundred
hosts and collapsed past it: one central editor as a bottleneck, no way to delegate, stale
copies everywhere, and name collisions with no authority to resolve them. DNS's insight was
hierarchy plus delegation — nobody needs to know everything, they only need to know who to
ask next.

**How does it work underneath?** Resolution is not one lookup. Your machine asks a
*recursive resolver* (your ISP's, or `1.1.1.1`), and if nothing is cached that resolver
walks down the tree:

- Ask a root server: who handles `.dev`? → a **referral** to the `.dev` nameservers
- Ask those: who handles `hooklens.dev`? → a **referral** to whatever nameservers the
  registrar has on file
- Ask *those*: what is the A record for `a7f3.hooklens.dev`? → the actual answer

Each step returns a pointer, not an answer, until the last one. Caching is the only reason
this is not unbearable — every record carries a TTL and resolvers hold it that long.

Record types that matter here: **A** (name → IPv4) and **AAAA** (→ IPv6); **CNAME** (name →
another name, an alias the resolver then resolves in turn); **TXT** (name → arbitrary text,
used by nothing for routing and existing purely to prove things); and the **wildcard**
`*.hooklens.dev`, which answers for any single label that has no more specific record.

**Sharp edges.**

- **Wildcards match exactly one label.** `*.hooklens.dev` covers `a7f3.hooklens.dev` and
  *not* `a.b.hooklens.dev` — the same rule that governs certificates.
- **A more specific record always wins.** Give `www` its own A record and the wildcard stops
  applying to it.
- **A CNAME cannot coexist with other records at the same name**, which is why you cannot
  CNAME an apex domain — it must carry NS and SOA. Every host's "ALIAS"/"ANAME" is a
  proprietary workaround for this.
- **TTL is a promise you cannot withdraw.** Set 86400 and that name cannot move for a day,
  because caches everywhere hold the old answer. You lower TTL *before* a migration.
- **"Propagation" is not a thing.** DNS never pushes. What people call propagation is caches
  expiring at their own pace.

**In hooklens.** Routing keys on the `Host` header, which is HTTP — but DNS is what causes
packets to arrive at all. A wildcard A record would land every `*.hooklens.dev` on our IP,
and only *then* does `Resolve` pick the inbox. The reason `a7f3.localhost` does not work on
the Windows dev machine is mundane: no resolver here answers for it. Hence the `/e/{slug}/`
path form in `internal/server/routing.go`.

## Brief — TLS

**What is it?** A protocol that turns a plain TCP connection into an encrypted,
authenticated one before a single byte of HTTP is sent.

It provides three things: confidentiality (nobody reads it), integrity (nobody alters it
undetected), and authentication (you are talking to who you think you are). Certificates
exist for the third, and it is the hard one — encryption to an impostor is worthless.

**What problem does it solve?** On plain HTTP every intermediary — your router, the café
wifi, the ISP, every hop — reads and can rewrite everything. Passwords in the clear,
injected ads, injected malware. And "just encrypt it" is not sufficient alone: you need a
shared key, and if you negotiate that key in the open, anyone in the middle can negotiate
one key with you and a different one with the server, decrypting and re-encrypting in
between while both sides see a padlock. That is the man-in-the-middle problem, and
authentication is the only thing that solves it.

**How does it work underneath?** The client sends a hello listing its cipher suites and —
this part matters — the hostname it wants, via **SNI**. That is what lets one IP address
serve many sites with different certificates. The server replies with its **certificate
chain**. Both sides then run a key exchange (ECDHE) producing a shared secret *neither of
them transmitted*. Everything after is ordinary symmetric encryption.

A certificate is a public key plus a list of names, signed by someone else. What it
**proves** is narrow and worth being precise about: that the holder controls the matching
private key, and that a CA was willing to attest they control those names. It proves nothing
about honesty, safety, or who the operator really is. A **CA** is an organisation whose
signing key ships in your operating system's trust store — your machine trusts a few hundred
by default. The chain runs leaf → intermediate → root, and the root is the one already on
your disk.

So how does a CA establish that you control a name? A challenge. ACME, the protocol Let's
Encrypt speaks, has two common ones:

- **HTTP-01** — the CA hands you a token; you serve it at
  `http://name/.well-known/acme-challenge/<token>`; the CA fetches that URL. Proves you
  control whatever the name currently points at.
- **DNS-01** — the CA hands you a token; you publish it as a TXT record at
  `_acme-challenge.name`; the CA looks it up. Proves you control the DNS *zone*.

**Why a wildcard requires DNS-01.** To get `*.hooklens.dev` you would have to demonstrate
control of every possible subdomain. There are infinitely many, the CA cannot pick a
representative one to test, and there is no URL to fetch for a name that does not exist yet.
But a wildcard is a property of the *zone*, so proving control of the zone is exactly the
right shape of proof. That is the whole reason wildcard issuance is DNS-01 only — and why it
needs API credentials for your DNS provider rather than just a running web server.

**Sharp edges.**

- Wildcards cover one label, matching the DNS rule.
- Validity comes from the **SAN** list. The Common Name is legacy and modern clients ignore
  it entirely.
- Certificates expire — Let's Encrypt issues for 90 days and the industry is moving shorter.
  Renewal has to be automated or you have scheduled an outage.
- SNI travels in the clear, so an observer learns which host you are visiting even though the
  content is hidden.
- A wrong or expired certificate stops a browser, but plenty of non-browser clients skip
  verification — so a route reachable only over a bad certificate looks fine to `curl -k` and
  broken to everyone real.

## Observed

No code in this unit, so instead of a walkthrough: everything claimed above, checked against
the real internet from this machine. Commands are reproducible.

### Delegation is real, and each step is a referral

```
$ curl -s "https://dns.google/resolve?name=dev&type=NS"
  ns-tld1.charlestonroadregistry.com.
  ns-tld3.charlestonroadregistry.com.
```

`.dev` is delegated to Google's registry nameservers. Asking those about `github.dev`
returns another referral rather than an address:

```
$ nslookup -type=ns github.dev
  github.dev  nameserver = dns1.p02.nsone.net      (and p02 dns2-4)
$ nslookup -type=a  github.dev
  github.dev  20.43.185.14                          <- only now, the answer
```

Three questions, two referrals, one answer.

### TXT records exist to prove things

```
$ curl -s "https://dns.google/resolve?name=google.com&type=TXT"
  "v=spf1 include:_spf.google.com ~all"
  "MS=E4A68B9AB2BB9670BCE15412F62916164C0B20BB"
  "docusign=05958488-4752-4ef2-95eb-aa7ba8a3bd0e"
```

None of these route anything. Two of the three are a third party saying "prove you own this
zone by publishing this string." DNS-01 is the same trick with `_acme-challenge` as the
name, which is why control of the zone is what a wildcard certificate actually attests to.

### A wildcard zone, answering for names nobody registered

```
1-2-3-4.sslip.io              -> 1.2.3.4
203-0-113-9.sslip.io          -> 203.0.113.9
deeply.nested.1-2-3-4.sslip.io-> 1.2.3.4
```

Worth a caveat rather than overclaiming: the third line resolving does **not** contradict
the one-label rule. sslip.io runs a custom nameserver that parses the name, not a plain `*`
record. A real `*.example.com` wildcard record answers for one label only. sslip.io is here
to show what "a zone that answers for arbitrary names" looks like in practice — it is the
shape `*.hooklens.dev` would need.

### Certificates: what is actually in one

```
$ echo | openssl s_client -connect github.com:443 -servername github.com \
    | openssl x509 -noout -subject -issuer -dates -ext subjectAltName

subject = CN=github.com
issuer  = Sectigo Public Server Authentication CA DV E36
notBefore = Sep  1 2026    notAfter = Nov 29 2026     <- 89 days
SAN     = DNS:github.com, DNS:www.github.com
```

Two things confirmed: validity is under 90 days even from a commercial CA, and the names
that count live in the **SAN** list. `CN=github.com` is duplicated there for legacy reasons
and modern clients ignore the CN entirely.

### The one-label rule, proven against a real certificate

`en.wikipedia.org` serves a Let's Encrypt certificate (also ~90 days: Aug 5 → Nov 3) whose
SAN list is an accidental masterclass. It contains, among forty entries:

```
DNS:wikipedia.org          <- the apex, listed explicitly
DNS:*.wikipedia.org
DNS:*.m.wikipedia.org      <- listed SEPARATELY
```

If a wildcard covered more than one label, `*.m.wikipedia.org` would be redundant — and if
`*.wikipedia.org` covered the bare domain, the apex entry would be too. Both are there
because neither is true. Verified directly with OpenSSL's own hostname matching:

```
MATCHES   wikipedia.org        (via the explicit apex SAN, NOT the wildcard)
MATCHES   en.wikipedia.org     (via *.wikipedia.org)
MATCHES   en.m.wikipedia.org   (via *.m.wikipedia.org, which they had to add)
REJECTED  a.b.wikipedia.org    <- no *.b.wikipedia.org exists, so: rejected
```

That last line is the rule. It is the same rule that makes
`Resolve("hooklens.dev", "a.b.hooklens.dev", "/")` return `TargetApp` in
`internal/server/routing.go:63` — the router and the certificate agree on what a subdomain
is, which is not a coincidence.

### SNI travels in the clear

```
$ openssl s_client -connect github.com:443 -servername github.com -trace

extension_type=server_name(0), length=15
  0000 - 00 0d 00 00 0a 67 69 74-68 75 62 2e 63 6f 6d   .....github.com
```

`github.com` as plaintext ASCII in the ClientHello, sent before any key exchange has
happened — so there is no encryption yet to hide it. An observer on the path learns which
host you asked for, even though everything after is opaque. (Encrypted Client Hello is the
in-progress fix; it is not universally deployed.)

### Two demonstrations that failed, and why

Honesty about method: **two attempts to show SNI selecting between different certificates on
one IP produced nothing.**

1. Cloudflare's `1.0.0.1` returned `CN=cloudflare-dns.com` for both `one.one.one.one` and
   `cloudflare-dns.com` — one certificate covers both names, so there was nothing to select
   between.
2. GitHub Pages' shared `185.199.108.153` returned `CN=*.github.io` for every `-servername`
   tried, and for no SNI at all — it serves one fallback certificate and terminates custom
   domains elsewhere.

Neither result contradicts the claim; they are just badly chosen targets. The claim that SNI
*selects* a certificate is therefore **taught but not demonstrated here**. What is
demonstrated is that SNI is sent, and sent unencrypted, which is the half that matters for
the sharp edge above.

## Consequences for hooklens

**This is why the `/e/{slug}/` path form exists.** Per-inbox subdomains need two things we do
not have: a wildcard A record in a zone we control, and a wildcard certificate — which,
per the above, can only be issued via DNS-01, which needs API credentials for the DNS
provider. The path form needs none of it and works on any hostname, including a free
platform subdomain.

**If a domain is ever bought**, the requirements are now concrete:

- a wildcard A (or ALIAS) record `*.domain` pointing at the server
- ACME with the DNS-01 challenge, not HTTP-01
- an API token for the DNS provider, held by whatever does the renewing
- automated renewal, because 90 days is an outage with a date on it

**The apex is a separate name.** `*.domain` does not cover `domain`, so the certificate needs
both — exactly as Wikipedia's does. Our router already treats them separately
(`routing.go:55` handles the apex and `www`; `routing.go:63` handles one label in front),
so the code and the certificate would line up without change.
