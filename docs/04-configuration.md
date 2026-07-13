# 04 — Configuration

Everything is an environment variable (`FABRIC_*`); flags override env; then
validation derives dependent values. There is no config file.

## The one required setting

| Env | Flag | Meaning |
|---|---|---|
| `FABRIC_ENTRA_ISSUER` | `-entra-issuer` | The exact `iss` bearer tokens must carry — an entra-emulator issuer (`https://entra-emulator:8443/{tenant}/v2.0`) **or a real Entra tenant** (`https://login.microsoftonline.com/{tenant}/v2.0`). Startup fails without it. |

## Everything else

| Env | Flag | Default | Meaning |
|---|---|---|---|
| `FABRIC_ADDR` | `-addr` | `:9443` | Listen address. |
| `FABRIC_DATA_DIR` | `-data-dir` | *(empty)* | SQLite + TLS state directory. **Empty = fully in-memory** (fresh DB and ephemeral cert per run — ideal for tests). The Docker image sets `/data`. |
| `FABRIC_ENTRA_JWKS_URL` | `-entra-jwks-url` | *derived* | Where signing keys are fetched. Derived from the issuer by the Entra convention: `{origin}/{tenant}/v2.0` → `{origin}/{tenant}/discovery/v2.0/keys`. Set explicitly only when the JWKS lives elsewhere (e.g. reachable under a different host than the advertised issuer). |
| `FABRIC_ENTRA_TLS_INSECURE` | `-entra-tls-insecure` | `false` | Skip TLS verification when fetching the JWKS — needed when entra-emulator serves its self-signed cert (the compose file sets it). Never needed against real Entra. |
| `FABRIC_DISABLE_TLS` | `-disable-tls` | `false` | Serve plain HTTP instead of self-signed TLS. Handy for curl exploration or behind a TLS-terminating proxy. |
| — | `-lro-delay` | `0` | Virtual seconds an async operation stays `Running` before succeeding. `0` = completes on the next poll. Combine with [clock control](10-testing.md) for deterministic polling tests. |

Booleans accept `1`, `true`, `yes`, `on` (case-insensitive); anything else is
false. `Retry-After` on `202` responses is fixed at 1 second.

## Real-compute sidecars

Empty by default — leave them unset and the emulator handles Spark/SQL with its
built-in fakes. Point them at running sidecars to route to real compute; see
[real compute](14-real-compute.md) and
[warehouse TDS](16-warehouse-tds.md) for the full setup.

| Env | Flag | Default | Meaning |
|---|---|---|---|
| `FABRIC_SPARK_LIVY_URL` | `-spark-livy-url` | *(empty)* | Livy endpoint for real Spark session/statement execution. |
| `FABRIC_SPARK_AGENT_URL` | `-spark-agent-url` | *(empty)* | Spark agent endpoint fronting the Livy cluster. |
| `FABRIC_SQL_TDS_ADDR` | `-sql-tds-addr` | *(empty)* | TDS listen address for the SQL analytics endpoint. |
| `FABRIC_WAREHOUSE_SQL_URL` | `-warehouse-sql-url` | *(empty)* | Backing SQL engine URL for warehouse query execution. |

## Subcommands

| Command | Does |
|---|---|
| `fabric-emulator version` | prints the release version (`dev` for source builds) |
| `fabric-emulator healthcheck` | probes `/health` on the local instance (honors `FABRIC_ADDR`), exit 0 when healthy — this is the Docker image's `HEALTHCHECK`, since distroless has no shell |

## Issuer alignment (the one subtle bit)

fabric-emulator compares the token's `iss` claim **string-exactly** against
`FABRIC_ENTRA_ISSUER`. Tokens carry whatever issuer entra-emulator *advertises*
(its login origin), which is not automatically the hostname you fetch the JWKS
from. On the compose network entra is therefore told to advertise the
network-internal origin:

```yaml
entra-emulator:
  environment:
    ORIGIN_MODE: compat
    PUBLIC_ORIGIN: "https://entra-emulator:8443"
```

so `iss` = `https://entra-emulator:8443/{tenant}/v2.0` = what fabric validates.
If tokens are rejected with an issuer mismatch, this alignment is almost always
the cause. See [TLS & hosts](05-tls-and-hosts.md) for the full story.
