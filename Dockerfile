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
FROM --platform=$BUILDPLATFORM golang:1.25 AS build
ARG VERSION=dev
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /fabric-emulator ./cmd/fabric-emulator
# Create the state dir here so it can be COPYed into the distroless image with
# nonroot ownership — distroless has no shell to mkdir/chown at runtime.
RUN mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /fabric-emulator /usr/local/bin/fabric-emulator
# State (SQLite + persisted TLS cert) lives here; mount to persist. It MUST be
# owned by the nonroot uid (65532) or the server can't open its SQLite DB —
# a fresh anonymous/named volume inherits this dir's ownership, so chown it.
COPY --from=build --chown=65532:65532 /data /data
ENV FABRIC_DATA_DIR=/data
VOLUME /data
EXPOSE 9443
# Distroless has no shell; the binary probes its own /health.
HEALTHCHECK --interval=10s --timeout=3s --retries=5 CMD ["/usr/local/bin/fabric-emulator", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/fabric-emulator"]
