# 02 — Installation

One static Go binary (pure Go — no CGO, no runtime dependencies), CI-tested on
Linux, macOS, and Windows.

> **Release channels activate at the first tagged release.** Homebrew, winget,
> the GHCR image, and the archive downloads below publish automatically from
> `v*` tags; until the first tag lands, install from source (`go install`
> works today).

## macOS — Homebrew

```bash
brew install calvinchengx/tap/fabric-emulator
```

## Windows — winget

```powershell
winget install calvinchengx.fabric-emulator
```

## Any platform — go install

```bash
go install github.com/calvinchengx/fabric-emulator/cmd/fabric-emulator@latest
```

Needs Go ≥ 1.25. Pure Go all the way down (`modernc.org/sqlite`), so this
cross-compiles and installs anywhere Go runs.

## Docker

```bash
docker run --rm -p 9443:9443 \
  -e FABRIC_ENTRA_ISSUER="https://host.docker.internal:8443/11111111-1111-1111-1111-111111111111/v2.0" \
  -e FABRIC_ENTRA_TLS_INSECURE=true \
  ghcr.io/calvinchengx/fabric-emulator:latest
```

Distroless, multi-arch (amd64/arm64), with a built-in `HEALTHCHECK` (the
binary probes its own `/health` — no shell in the image). State lives in
`/data`; mount it to persist.

## docker-compose — the emulator pair, with real engines by default

```bash
docker compose up
```

This starts entra-emulator + fabric-emulator wired together (issuer alignment
included), **plus real compute attached by default**: a Spark statement-executor
agent (native Livy sessions, notebook cell execution) and a SQL Server sidecar
(the T-SQL/TDS warehouse surface — Warehouse, Lakehouse SQL endpoint, Fabric SQL
Database). `docker-compose.override.yml` — auto-loaded alongside
[`docker-compose.yml`](../docker-compose.yml), no flag needed — adds those
sidecars and their env vars; see [14-real-compute.md](14-real-compute.md).

Opt out to the lite, contract-only pair (no heavy sidecars, honest 501s on the
Spark/SQL surfaces) by naming the base file explicitly, which makes Compose skip
the override:

```bash
docker compose -f docker-compose.yml up
```

This is the recommended way to run the full stack — see the
[quickstart](01-quickstart.md).

## Release archives

Tagged releases carry `tar.gz`/`zip` archives per OS/arch plus
`checksums.txt`: <https://github.com/calvinchengx/fabric-emulator/releases>.

## From source

```bash
git clone https://github.com/calvinchengx/fabric-emulator
cd fabric-emulator
go build ./cmd/fabric-emulator
```

## Verify

```bash
fabric-emulator version      # stamped by the release pipeline; "dev" from source
fabric-emulator healthcheck  # exit 0 when a local instance is healthy
```

The server needs one thing to start: an issuer to trust
(`FABRIC_ENTRA_ISSUER` or `-entra-issuer`) — see
[configuration](04-configuration.md).
