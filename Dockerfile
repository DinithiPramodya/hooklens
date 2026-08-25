# syntax=docker/dockerfile:1

# ---------- build stage ----------
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependency manifests first, on their own layer. This is the layer-cache edge
# from the brief: `go mod download` only re-runs when go.mod/go.sum change, so
# editing a .go file does not re-download the module graph. Copying everything up
# front would invalidate this layer on every single source edit.
COPY go.* ./
RUN go mod download

COPY . .

ARG VERSION=dev
# CGO_ENABLED=0 produces a statically linked binary with no libc dependency,
# which is what lets the runtime stage be an image containing almost nothing.
# -trimpath strips local filesystem paths out of the binary so the build is
# reproducible and does not leak "D:\Projects\..." into stack traces.
# -s -w drop the symbol table and DWARF data: smaller binary, no debugger.
RUN CGO_ENABLED=0 go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/hooklens ./cmd/hooklens

# ---------- runtime stage ----------
# distroless/static has no shell, no package manager, and no libc -- just CA
# certificates, timezone data, and /etc/passwd. Nothing to exploit because
# there is nothing there. Alpine would have been ~8MB with a full busybox
# shell; this is ~2MB with no shell at all.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/hooklens /hooklens

# Documentation only -- EXPOSE publishes nothing by itself. It tells readers and
# tooling which port the process listens on.
EXPOSE 8080

USER nonroot:nonroot

# ENTRYPOINT in exec form, not shell form. Shell form would wrap the binary in
# /bin/sh -c, which this image does not have -- and even where it exists, the
# shell becomes PID 1 and swallows SIGTERM, so the graceful shutdown we wrote in
# main.go would never fire.
ENTRYPOINT ["/hooklens"]
