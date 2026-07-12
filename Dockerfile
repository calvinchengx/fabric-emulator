# Build: pure Go (modernc sqlite, no CGO) → distroless static.
FROM golang:1.25 AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /fabric-emulator ./cmd/fabric-emulator
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
