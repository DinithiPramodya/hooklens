# 02 — Containers, images, and Compose

*Phase 0, unit 3: local infrastructure. Covers the curriculum bullet "containers: images
vs. containers, what Compose adds".*

## Brief

**What is it?** A container is an ordinary process running on the host kernel that has been
lied to about what it can see.

That is worth unpacking, because the common mental model — "a lightweight VM" — is wrong in
a way that causes real confusion later. There is no guest operating system, no emulated
hardware, no second kernel. It is `/usr/bin/hooklens` running as a process on the same
kernel as everything else, with two Linux features applied to it:

- **Namespaces** control what the process can *see*. Separate namespaces for process IDs
  (it gets its own process tree and believes it is PID 1), mounts (its own filesystem
  root), network (its own interfaces and its own port space), users, and hostname. The
  process looks around and sees a machine that does not exist.
- **cgroups** control what the process can *use* — ceilings on memory, CPU, and I/O.

An **image** is the filesystem a container starts from, plus metadata: which command to
run, which environment variables, which ports. Images are built in layers, one per
Dockerfile instruction, and layers are content-addressed, cached, and shared between
images. The relationship is class to instance: one image, fifty containers, stored once.

**What problem does it solve?** "Works on my machine." Before containers you wrote your
dependencies in a README and hoped. Deployment meant a server configured by hand, drifting
quietly from every other server. Two costs: environment drift — production has one library
version, your laptop another, and the difference surfaces at 2am — and setup cost, where a
new person spends a day getting Postgres running before writing a line of code. Virtual
machines did solve this, by shipping an entire operating system: gigabytes, and tens of
seconds to boot. Containers ship only the filesystem and borrow the host's kernel:
megabytes, and milliseconds.

**How does it work underneath?** `docker run` unpacks the image's layers into a union
filesystem — read-only layers with one thin writable layer on top — then calls `clone()`
with namespace flags set, configures cgroups, changes root into the unpacked filesystem,
and execs your command. That is it. Run `ps` on the host and the process is right there in
the list.

One consequence matters immediately: containers use the **host's** kernel. Linux containers
therefore cannot run natively on Windows, because there is no Linux kernel to borrow.
Docker Desktop's answer is a WSL2 virtual machine — a real Linux kernel in a lightweight
VM, with the Docker daemon living inside it. That is why Docker Desktop demanded
virtualization support, and why its error message was about virtualization rather than
about the actual missing piece, WSL.

**Sharp edges.**

- **Layer caching is order-dependent.** Copy your source code before installing
  dependencies and every one-character change re-downloads everything. Copy the dependency
  manifest first, install, *then* copy source.
- **Containers are ephemeral.** That thin writable layer dies with the container. Anything
  that must survive belongs in a volume — for us, Postgres's data directory.
- **PID 1 gets no default signal handling.** Normally the kernel gives PID 1 special
  treatment; your process as PID 1 must handle `SIGTERM` itself, or `docker stop` waits ten
  seconds and then `SIGKILL`s it. We already handle SIGTERM in `cmd/hooklens/main.go` —
  this is the reason it mattered.
- **A container is not a security boundary** the way a VM is. Shared kernel means a kernel
  bug is an escape.

**What Compose adds.** One YAML file describing several containers, the network they share,
and their volumes — instead of long `docker run` invocations you have to remember. The part
that earns its keep is DNS: services reach each other by name, so the app connects to
Postgres at the hostname `db`.

**In hooklens.** Compose runs Postgres for development. A separate multi-stage Dockerfile
builds the app itself for CI and deployment.

## Decisions

**Compose runs Postgres only; the app is not a service in it.**
Alternatives were putting the app in Compose too (one `up` starts everything), or skipping
Compose and installing Postgres natively on Windows. Chose Postgres-only because
`go run ./cmd/hooklens` rebuilds in under a second while a Docker image build costs tens of
seconds, and the app has no dependency on the Linux environment — so containerising it in
the edit loop buys nothing and taxes every change. The Dockerfile still exists and is built
in CI, so the image is never untested; it is just not in the way.
*Wrong call if* the app grows a dependency that is painful on Windows (a C library, a
specific libc). *The signal to switch* is the first "it only breaks on Linux" bug.

**Named volume, not a bind mount to a host directory.**
Two reasons, both specific to this machine. A bind mount would cross the Windows/WSL2
filesystem boundary, which is slow enough that Postgres notices. And it carries uid/gid
mismatches the `postgres` user trips over. A named volume lives inside the VM's own
filesystem. *Wrong call if* you wanted to inspect the data files directly from Windows
Explorer — but you never should.

**Port published on `127.0.0.1:5432`, not `5432:5432`.**
The short form binds `0.0.0.0` — every interface. On a laptop that means anyone on the same
café wifi can reach a Postgres whose password is literally `hooklens`. Binding the loopback
address costs nothing and closes it.

**`postgres:18-alpine`, pinned to a major version.**
Not `postgres:latest`, which silently becomes a new major version one day and refuses to
start against an existing data directory. Alpine over the Debian variant for size. Pinning
the *major* only (not `18.6`) so patch releases arrive without a code change.

**distroless/static for the runtime stage, not Alpine or scratch.**
Alpine would be ~8MB and includes a full busybox shell — a shell is exactly what an
attacker who achieves RCE wants. `scratch` has no shell but also no CA certificates and no
`/etc/passwd`, so TLS calls fail and there is no non-root user to switch to.
`distroless/static:nonroot` is the middle: CA certs, timezone data, a `nonroot` user, and
no shell. Final image 14.7MB.

**`CGO_ENABLED=0`.**
Produces a statically linked binary with no libc dependency, which is the thing that makes
a near-empty runtime image possible at all. It is also why `go test -race` does not run on
this machine — the race detector requires cgo and there is no C compiler on Windows. CI
runs it on Linux.

**Exec-form `ENTRYPOINT ["/hooklens"]`, not shell form.**
Shell form wraps the command in `/bin/sh -c`, which this image does not contain. Worse,
where a shell does exist it becomes PID 1 and does not forward SIGTERM, so `docker stop`
would hang for its full 10-second grace period and then SIGKILL — and the graceful drain
written in `cmd/hooklens/main.go` would never run. Measured: 716ms to stop. A broken setup
measures ~10,000ms, which is how you detect this without reading any code.

**Volume mounted at `/var/lib/postgresql`, not `/var/lib/postgresql/data`.**
This one was found by failing, not by planning — the container crash-looped on first start.
The Postgres 18 images changed convention: data now lives in a major-version subdirectory
(`/var/lib/postgresql/18/docker`) so a future `pg_upgrade --link` can see both versions
inside a single mount point. Mounting at the old path makes the image detect a stray
volume and refuse to start. Every pre-18 tutorial on the internet still shows the old path.

## Walkthrough

### `compose.yaml`

`name: hooklens` at the top sets the project name explicitly. Without it Compose derives one
from the directory — which here is `webhook-inspector`, so containers and volumes would be
named after a directory we may well rename.

The `environment` block: `POSTGRES_USER`, `POSTGRES_PASSWORD` and `POSTGRES_DB` are read by
the image's entrypoint script, not by Postgres itself, and **only on first start** when the
volume is empty. Changing the password later does nothing until you `down -v`. That
surprises people.

`POSTGRES_INITDB_ARGS: "--locale=C --encoding=UTF8"` is passed to `initdb`. `C` collation
sorts by raw byte value rather than by language rules — faster, and correct here because
nothing in this database is sorted for human reading.

The `healthcheck` is load-bearing, not decoration. `pg_isready` exits non-zero until the
server accepts connections. Without it, `depends_on` waits only for the *container* to
start, which happens several seconds before Postgres is actually listening — the classic
"connection refused on first run, works on retry" bug. It is also what makes
`docker compose up --wait` meaningful: that flag blocks until health checks pass, which is
what a CI script needs.

### `Dockerfile`

Two stages. The build stage has the whole Go toolchain (~250MB); the runtime stage gets
only the compiled binary. `COPY --from=build` is the line that discards everything else.

`COPY go.* ./` then `RUN go mod download` **before** `COPY . .` is the layer-cache edge from
the brief, made concrete. Layers are cached by the content of what they copy, so the
download layer is only invalidated when `go.mod`/`go.sum` change. Reverse those two blocks
and every one-character source edit re-downloads the module graph. Right now we have zero
dependencies so it costs nothing — the point is that it will silently cost minutes per
build from Phase 1 onward if it is wrong, and nobody will notice why.

`-trimpath` strips local filesystem paths from the binary: reproducible builds, and no
`D:\Projects\...` leaking into a stack trace. `-s -w` drop the symbol table and DWARF data.
`-X main.version=${VERSION}` writes into the `version` variable declared in
`cmd/hooklens/main.go` — that is how a build stamps itself without a generated file.

`EXPOSE 8080` publishes nothing. It is metadata for humans and tooling; `-p` is what
actually publishes.

`USER nonroot:nonroot` comes from the distroless image, which ships that user in
`/etc/passwd`. Running as root inside a container is not catastrophic on its own, but it
removes one layer for free.

### `.dockerignore`

Everything listed is excluded from the build *context* — the tarball uploaded to the daemon
on every build. Two benefits: smaller uploads, and fewer spurious cache invalidations,
since a change to a file in the context can bust a layer even when no `COPY` references it.
`.git` matters most; it grows without limit.

### `.gitattributes`

`* text=auto eol=lf`. Files are stored in the repository with LF and checked out however
the platform prefers. Without it, files committed from Windows carry CRLF into the
repository, and `gofmt` on a Linux CI runner reports every file as unformatted — a failure
that cannot be reproduced locally, which is the worst kind.

## Verified

- `docker compose up -d --wait` → healthy in ~6s
- PostgreSQL 18.6, `data_directory = /var/lib/postgresql/18/docker`
- `uuidv7()` available natively — confirms the assumption in PLAN.md's data model
- `bytea` write/read roundtrip clean
- Data survives `docker compose down` + `up`; destroyed by `down -v`, as intended
- `docker build` → 14.7MB image
- Containerised app serves `/healthz` and captures on `/e/a7f3/webhook`
- `docker stop` returns in **716ms**, proving SIGTERM reaches the process (a broken
  ENTRYPOINT or missing signal handling measures ~10,000ms)
