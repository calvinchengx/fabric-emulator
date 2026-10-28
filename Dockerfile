# Build: pure Go (modernc sqlite, no CGO) → distroless static.
#
# --platform=$BUILDPLATFORM pins the build stage to the machine doing the
# building, and GOOS/GOARCH then cross-compile to the requested target. Without
# it, `--platform linux/arm64` on an amd64 runner emulates the *whole toolchain*
# under QEMU just to run the compiler — the release workflow builds
# linux/amd64,linux/arm64, so one of the two was always paying that.
#
# This is safe here precisely because the binary is pure Go with CGO_ENABLED=0:
# cross-compiling needs no target-arch C toolchain, and `go build` emits the
# target's code at the builder's native speed.
#
# TARGETOS/TARGETARCH are supplied automatically by BuildKit. They are empty
# under the legacy builder, where empty GOOS/GOARCH correctly mean "host".
FROM --platform=$BUILDPLATFORM mirror.gcr.io/library/golang:1.27@sha256:b76020929b709ddb6202afc05410a0faefae20ae05e27ad548b3870092688a69 AS build
ARG VERSION=dev
# The image is what serves the portal, so it carries the same identity the
# released binaries do. Empty is honest for a local `docker build`.
ARG COMMIT=
ARG TARGETOS TARGETARCH
# COVER=1 builds an instrumented binary that writes coverage counters to
# $GOCOVERDIR when it exits cleanly, so the e2e fleet can report what it
# actually exercised instead of being invisible to the coverage number
# (docs/10-testing.md). Off by default: instrumentation costs runtime and the
# published image must not carry it.
ARG COVER=
WORKDIR /src
COPY go.mod go.sum ./
# Retried, bounded, because proxy.golang.org drops HTTP/2 streams mid-transfer
# often enough to matter: 2026-10-25 lost a CI job to four modules failing at
# once with `stream error: stream ID 1; INTERNAL_ERROR; received from peer`,
# nothing to do with this repository. A real failure (a bad go.sum, a module
# that is gone) fails all four attempts and the build still stops here, with
# go's own message as the last thing printed.
RUN for attempt in 1 2 3 4; do \
      go mod download && exit 0; \
      [ "$attempt" = 4 ] && break; \
      echo "go mod download failed (attempt $attempt/4); retrying in $((attempt * 15))s" >&2; \
      sleep $((attempt * 15)); \
    done; \
    echo "go mod download failed 4 times — giving up" >&2; exit 1
COPY . .
# -coverpkg=./... is what makes this measure the SERVER rather than only the
# main package: without it an instrumented binary reports coverage for
# cmd/fabric-emulator alone, which is the one package an e2e barely touches.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build ${COVER:+-cover -coverpkg=./...} \
    -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /fabric-emulator ./cmd/fabric-emulator
# Create the state dir here so it can be COPYed into the distroless image with
# nonroot ownership — distroless has no shell to mkdir/chown at runtime.
RUN mkdir /data /covdata

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /fabric-emulator /usr/local/bin/fabric-emulator
# State (SQLite + persisted TLS cert) lives here; mount to persist. It MUST be
# owned by the nonroot uid (65532) or the server can't open its SQLite DB —
# a fresh anonymous/named volume inherits this dir's ownership, so chown it.
COPY --from=build --chown=65532:65532 /data /data
# Present in every image so the coverage overlay can mount over it; an
# uninstrumented binary ignores GOCOVERDIR entirely, so this costs an empty
# directory and nothing else.
COPY --from=build --chown=65532:65532 /covdata /covdata
ENV FABRIC_DATA_DIR=/data
VOLUME /data
EXPOSE 9443
# Distroless has no shell; the binary probes its own /health.
HEALTHCHECK --interval=10s --timeout=3s --retries=5 CMD ["/usr/local/bin/fabric-emulator", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/fabric-emulator"]
