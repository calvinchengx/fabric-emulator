# 21 — Design: one toggle between fabric-emulator and real Fabric

**Status: T0 shipped** (`python/fabric-target/`, CI `fabric-target`); T1–T2 remain design. Goal: a user's Python functionality — SDK calls,
`fabric-cicd` pipelines, notebooks on the `notebookutils` shim, dbt projects,
plain `requests` — runs against **either** the local emulator family **or**
the real Fabric service, switched by **one setting**, with zero code edits.

## Why this is nearly free

Everything this project did to run *real clients unmodified against the
emulator* is exactly the machinery a toggle needs, pointed the other way:

- Every client is already parameterized by an **API root**
  (`FABRIC_API_ROOT_URL` / `DEFAULT_API_ROOT_URL`), a **token authority +
  credential** (azure-identity), a **storage endpoint** (OneLake), and a
  **vault URL** — because that's how the e2es aim them at localhost.
- The emulator deliberately speaks v2.0/JWKS/challenge auth exactly like
  production, so azure-identity, MSAL, and the SDKs cannot tell the
  difference — only the endpoints and the credential values change.

So the toggle is not an emulation feature; it is **one resolver** that turns a
target name into a coherent set of endpoints + credentials, plus guardrails
for the places the two worlds genuinely differ.

## The contract

One switch: `FABRIC_TARGET=emulator | real` (default `emulator`).

| Resolved value | `emulator` (zero-config defaults) | `real` (from standard env) |
|---|---|---|
| API root | `https://localhost:9443/v1` | `https://api.fabric.microsoft.com/v1` |
| Token authority | entra-emulator (`https://localhost:8443/{tenant}`) | `https://login.microsoftonline.com/{AZURE_TENANT_ID}` |
| Credential | seeded daemon SP (`cccccccc-…0002` / `daemon-app-secret`) | `AZURE_CLIENT_ID`/`AZURE_CLIENT_SECRET`, or `DefaultAzureCredential` (az CLI, managed identity, browser) |
| OneLake | `https://localhost:9443` + Host/`--resolve` (or `az://` via Sail) | `https://onelake.dfs.fabric.microsoft.com` |
| Key Vault | azure-keyvault-emulator (`https://localhost:8444`) | the user's real vault URI |
| TLS verify | off (self-signed family certs) | on |
| Workspace | by id **or name** against the emulator | **by name** (`FABRIC_WORKSPACE`), resolved to the real GUID at startup |

Ids are the one thing that can never match across targets — so the contract
is **name-based**: user code holds workspace/item display names; the resolver
translates to GUIDs per target.

## The family — trust direction constrains the toggle

Tokens flow one way: entra (or real AAD) **issues**; fabric and keyvault only
**validate**. That means the toggle is not three independent switches — only
chains rooted in a single issuer are coherent:

| Combination | Works? | Why |
|---|---|---|
| All-emulator | ✅ | the default family — one trust chain rooted in entra-emulator |
| All-real | ✅ | real AAD issues; real Fabric + real Key Vault validate |
| Real AAD + emulated fabric/keyvault (**hybrid**) | ✅ | both emulators already accept any configured issuer — `FABRIC_ENTRA_ISSUER` / `KV_ENTRA_ISSUER` can point at `login.microsoftonline.com/{tenant}` today |
| Emulated entra + real fabric/keyvault | ❌ | real Microsoft services will never trust the emulator's JWKS |

So `FABRIC_TARGET` flips the whole chain; the one supported refinement is a
**hybrid profile** (`FABRIC_TARGET=emulator` + `AUTH_TARGET=real`) for teams
with a real AAD app registration but no Fabric capacity.

Per member, "real" resolves as:

- **entra-emulator → real Entra ID.** azure-identity *is* the toggle: the
  resolver builds `ClientSecretCredential(..., authority=<entra-emulator>)`
  with the seeded SP in emulator mode, and **exactly
  `DefaultAzureCredential()`** in real mode. Its chain order does the rest
  with no branching of ours: explicit `AZURE_TENANT_ID`/`AZURE_CLIENT_ID`/
  `AZURE_CLIENT_SECRET` win when set (CI, service contexts); otherwise
  **`az login` wins** — tokens are minted from the developer's own CLI
  session (delegated identity; workspace RBAC and Conditional Access apply
  to *them*), refresh handled by azure-identity re-invoking `az`. All three
  family scopes (Fabric, Storage, Vault) mint through the CLI. Non-Python
  tools follow the same split via the env emitter: `fabric-cicd` already
  uses `DefaultAzureCredential` internally, `dbt-fabric` takes
  `authentication: CLI`, `azcopy` takes `AZCOPY_AUTO_LOGIN_TYPE=AZCLI`.
- **azure-keyvault-emulator → the user's actual vault.** Key Vault's
  **challenge-based auth does the discovery**: the SDK hits the vault, reads
  the 401 `WWW-Authenticate` challenge naming the authority, and follows it —
  the AKV emulator implements that same challenge advertising entra-emulator's
  authority, so identical `SecretClient(vault_url, credential)` code walks
  either chain. The resolver supplies only the **vault URL** per target
  (`https://localhost:8444` and its default vault vs
  `https://{name}.vault.azure.net`). Vault **names** are the cross-target
  contract, exactly like workspace names; `AzureKeyVaultReference`
  connections carry the same shape on both sides.
- **Seeded values never leak into real mode.** Tenant `11111111-…`, the
  daemon SP, and `daemon-app-secret` are emulator-mode defaults only; in real
  mode the resolver requires a **real credential source** — env SP vars *or*
  a live `az login` (the `DefaultAzureCredential` chain probe) — and refuses
  to fall back to seeds. No source found → fail at startup with "run
  `az login` or set AZURE_* credentials", never a silent seed.

## Deliverable A — `fabric_target` (Python helper, `python/fabric_target/`)

Small sibling of the `notebookutils` shim, same env-driven style:

```python
from fabric_target import target

t = target()                     # reads FABRIC_TARGET, resolves the profile
t.credential                     # azure.identity credential for this target
t.session()                      # requests.Session: base URL, bearer auth,
                                 # verify flag, retry-on-429 — same object
                                 # whichever target is active
ws = t.workspace("analytics")    # name → id, either target
t.session().post(f"/workspaces/{ws.id}/items", json={...})

t.onelake                        # adlfs/azure-storage-blob-ready endpoint + credential
t.vault_url                      # keyvault base for this target
t.emulator_only("clock freeze")  # raises TargetError under FABRIC_TARGET=real
```

Implementation notes: authority override is plain azure-identity
(`ClientSecretCredential(..., authority=...)` — the e2es already prove entra
works as an authority); `verify=False` only in emulator mode; the profile is
resolved once and printable (`python -m fabric_target show`).

## Deliverable B — env emitter (non-Python tools, one command)

The same resolver, exported for tools that only read env — `fabric-cicd`,
dbt profiles, azcopy, the `notebookutils` shim, `fab` CLI:

```bash
eval "$(python -m fabric_target env real)"      # or: emulator
```

Emits the full coherent set: `FABRIC_API_ROOT_URL`, `DEFAULT_API_ROOT_URL`,
`AZURE_TENANT_ID`/`AZURE_CLIENT_ID`/…, `NOTEBOOKUTILS_*` (mapped onto real
endpoints in real mode), `REQUESTS_CA_BUNDLE`/`SSL_CERT_FILE` handling, and —
emulator mode only — the DNS-pin guidance for hostname-strict tools
([05-tls-and-hosts.md](05-tls-and-hosts.md)).

## Guardrails — where the worlds differ on purpose

The toggle must make these differences *loud*, not paper over them:

1. **Emulator-only surfaces hard-fail in real mode.** `/_emulator/*` (clock,
   faults, portal data), forged tokens, seeded principals: the helper's
   `emulator_only()` raises `TargetError("clock control does not exist on
   real Fabric")` rather than letting a test silently no-op.
2. **Time is real.** No frozen clock: LROs poll for real minutes; the helper's
   `session()` bakes in `Retry-After`-honoring polling either way, so code
   written against the emulator's instant LROs still behaves.
3. **Real mode costs money and touches real state.** Destructive verbs
   (workspace/item DELETE) require `FABRIC_TARGET_ALLOW_DESTRUCTIVE=1` in
   real mode; the resolver refuses to start in real mode without an explicit
   `FABRIC_WORKSPACE` scope, so nothing ever enumerates a whole tenant.
4. **Throttling exists.** 429/`Retry-After` handling is on by default in the
   session (the emulator can rehearse it via fault injection).
5. **RBAC is real.** The SP needs actual workspace roles; the resolver's
   startup probe (`GET /workspaces` + the scoped workspace) fails fast with
   a "grant your SP access" message instead of 403s mid-run.

## Verification — the same tests, both targets

A pytest marker ties it together:

```python
@pytest.mark.target          # runs under either FABRIC_TARGET
def test_publish_roundtrip(t): ...
```

- **CI (every push):** the marked suite runs with `FABRIC_TARGET=emulator` —
  free, deterministic, offline.
- **Nightly / manual (`workflow_dispatch`):** the same suite with
  `FABRIC_TARGET=real`, gated on repo secrets (`AZURE_TENANT_ID`, SP creds,
  a dedicated throwaway workspace). Every divergence found feeds the
  [parity map](../parity.md) — the toggle doubles as a **fidelity oracle**:
  the emulator's behavior is continuously diffed against the real service.

## Phasing

| Phase | Lands | Proves |
|---|---|---|
| **T0** ✅ | `python/fabric-target/` (resolver, TokenCredential-shaped emulator credential, guarded session + LRO poll, `env` emitter) + unit tests + `e2e/fabric-target` (CI 3-OS) + quickstart section | toggle commands are real; emulator profile CI-verified end to end |
| **T1** | pytest `target` marker + the secret-gated `real-fabric` workflow | same suite green on both; divergences filed against the parity map |
| **T2** | `notebookutils` real mode (`NOTEBOOKUTILS_*` resolved from the real profile: real OneLake, real vault, `DefaultAzureCredential`) | notebook code runs unchanged locally *and* as a genuine Fabric notebook |

## Non-goals

Proxying or recording real Fabric traffic through the emulator, translating
ids between targets persistently (names are the contract), emulating tenant
onboarding/capacity purchase, and hiding real-mode latency or cost.
