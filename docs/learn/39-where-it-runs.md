# 39 — Where it runs: a platform vs. a box you own

> Phase 6 · unit 39. Dogfooding needs a permanent address. This is the choice of who
> operates the machine behind it, and what the repo has to say about it.

## Brief

**What is this thing?** Deployment is putting the binary on a computer that is
always on, has a public address, and lets strangers reach it. *Deploy config* is
the file in the repo that describes that machine, so the setup is a thing anyone
can repeat rather than a sequence of commands somebody once typed.

*Dogfooding* is the reason we want it now: pointing this repository's own GitHub
webhooks at the running instance and leaving it there. It is the cheapest honest
test there is. A tool you use daily surfaces the bugs a test suite cannot imagine —
the certificate that expires, the tunnel that dies quietly after six hours, the
retention sweep that deletes something it should not.

**What problem does it exist to solve?** The old way was to SSH into a server and
copy files by hand. It worked until it did not: nobody could say what was actually
installed, the machine drifted away from every other machine, and rebuilding it
after a dead disk meant reconstructing it from memory. Writing the config down
makes the server reproducible, reviewable and diffable — the same reason the schema
lives in `migrations/` instead of in whoever ran `psql` last.

**How does it actually work underneath?** Both options end in the same place: a
Linux machine running our container, holding a public IP, terminating TLS, talking
to a Postgres. The difference is only *who operates each layer*.

*A platform (Fly.io).* We hand over an image and a small config file. Their system
runs it on a machine we never see, restarts it when it dies, holds the TLS
certificate, and rents us a Postgres with backups already configured. We operate
the application and nothing else.

*A box we rent (a VPS).* We get a bare Linux machine and an IP address. We install
Docker, run our container, and put Caddy in front as a reverse proxy. Caddy performs
the ACME DNS-01 exchange itself — proving control of the domain by writing a TXT
record through Cloudflare's API — to obtain the `*.hooklens.dev` wildcard. We run
Postgres, and we own its backups, its upgrades, and the kernel patches on the box.

The wildcard is the part that forces DNS-01 either way: a certificate authority
will not accept the HTTP challenge for a `*` name, because serving a file at one
hostname proves nothing about the infinite set of others. See note 05.

**What are the sharp edges?**

- **The database is the bill.** The application is a small static binary; a managed
  Postgres is most of the monthly cost on any platform.
- **One instance, not two.** The tunnel hub holds its state in memory, so a request
  arriving at instance B cannot reach a CLI connected to instance A
  (`internal/server/server.go:30-33`). Whatever we deploy runs single-replica until
  that changes — which rules out anything that quietly autoscales.
- **Scale-to-zero breaks capture.** A platform that sleeps an idle instance misses
  deliveries while it wakes. Providers retry, then start disabling endpoints. For
  an inbox that exists to be always reachable, sleeping is a correctness bug.
- **The DNS token is a dangerous credential.** DNS-01 needs an API token that can
  edit records for the domain — which means it can also repoint the domain. Scope it
  to one zone and store it as a secret, never in the repo.
- **A box you own is a box you own forever.** The €4/month is the small part. The
  standing cost is patching, backups you actually test, and being the person paged
  when the disk fills.

**Here is how this shows up in hooklens.** `PLAN.md:81` already names Fly plus
Cloudflare DNS as the choice, with the VPS listed as the cheaper and more
educational alternative — and Phase 0's goal was written assuming `*.hooklens.dev`
points at Fly. Nothing is deployed yet, so either is still open; the cost of
switching later is one config file and a DNS change, not a rewrite, because the
binary is the same artifact in both worlds.

---

## Decisions

**Not deploying, at all, for now.** The author ruled it out: hosting costs money, and
this is a portfolio project with no revenue to pay for it. That is a legitimate
constraint and it decides the question — neither Fly nor the VPS gets built.

It is worth being exact about what that gives up, because two of Phase 6's goals
assumed a hosted instance:

- **Dogfooding.** `PLAN.md` wanted the author's own repositories sending real GitHub
  webhooks to a long-running instance — the most credible line a README can carry,
  and the best source of bugs no test anticipates. Not happening.
- **The "done when".** Phase 6's bar is *a stranger goes from landing page to their
  first captured webhook in under 60 seconds*. With no hosted landing page, the
  closest achievable version is *from a fresh clone*, and that part was verified:
  the committed tree, exported with `git archive`, runs the README quickstart as
  written through to a webhook forwarded to a local app and relayed back
  ([35](35-release-and-distribution.md)). It takes several minutes, not 60 seconds,
  and assumes Go, Node and Docker. That gap is real and is the cost of the decision.

**The zero-cost path, recorded for later.** GitHub's webhooks need a public HTTPS
URL, but that does not require renting anything. Run hooklens locally and expose it
through a free tunnel service, and point a webhook at the `/e/{slug}/` path form —
which needs no wildcard DNS and no certificate of our own, sidestepping the DNS-01
question in the brief entirely. The trade-offs are that the public URL is usually
random and changes on restart (so the webhook must be edited each time), and uptime
is only as good as the laptop. Enough for a week of dogfooding and a recording for
the demo; not a hosted product.

**The signal to revisit** is either a reason to spend money — someone wants to use a
hosted instance — or a free tier that runs a single always-on instance with Postgres
and does not scale to zero. Scale-to-zero stays disqualifying for the reason the
brief gives: a sleeping capture endpoint misses deliveries.

No walkthrough: nothing was built.
