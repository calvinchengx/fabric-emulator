# 11 — How-to: run the real fabric-cicd against the emulator

[`fabric-cicd`](https://github.com/microsoft/fabric-cicd) is Microsoft's
Python deployment tool for Fabric. It runs against the emulator **unmodified**
— which means your deployment scripts and pipelines built on it can be tested
offline, with no capacity and no tenant. This page walks the working example
in [`e2e/fabric-cicd/`](../e2e/fabric-cicd/), which CI runs on every push.

## Try it

```bash
python3 e2e/fabric-cicd/run.py
```

Self-contained: installs entra-emulator (`go install`) if missing, builds
fabric-emulator from the repo, creates a venv with `fabric-cicd`, publishes a
notebook, and asserts the definition round-tripped.

## How it works — the three tricks

**1. fabric-cicd's own URL overrides.** The tool reads two env vars; no fork
needed:

```bash
FABRIC_API_ROOT_URL="https://api.fabric.microsoft.com:$FABRIC_PORT"
DEFAULT_API_ROOT_URL="https://api.fabric.microsoft.com:$FABRIC_PORT"   # its Power BI root
```

Its URL validator insists on the *hostname* `api.fabric.microsoft.com` but
accepts any port — which enables:

**2. An in-process DNS pin.** Before anything opens a socket,
[`driver.py`](../e2e/fabric-cicd/driver.py) monkey-patches
`socket.getaddrinfo` to resolve the Fabric hostnames to `127.0.0.1` (a
process-scoped `curl --resolve`). The emulator's self-signed cert covers
`api.fabric.microsoft.com` ([TLS & hosts](05-tls-and-hosts.md)), so TLS
handshakes succeed under the real name.

**3. Real tokens from entra-emulator.** The driver authenticates with a
seeded service principal via azure-identity's `ClientSecretCredential`
pointed at entra-emulator — the same client-credentials flow the tool uses in
production.

## What the driver does

1. Creates a workspace as the SP (creator → Admin; the seeded capacity keeps
   fabric-cicd's `capacityId` check happy).
2. `FabricWorkspace(workspace_id=…, repository_directory=…)` +
   `publish_all_items(ws)` — the tool's real publish path, pushing a notebook
   with `.platform` + `notebook-content.py` parts.
3. Reads back `/items` and `getDefinition`, asserting the parts round-tripped
   byte-for-byte.

## What driving the real tool caught

Running Microsoft's actual client surfaced fidelity gaps no spec-reading
would have: the `/v1/workspaces/{id}/folders` endpoint (now implemented),
`description` must always be present on item wire shapes, result-less LROs
must not advertise a result `Location`, and fabric-cicd refuses workspaces
with no `capacityId` (hence the seeded default). That is the point of
real-tool e2e — see the [e2e matrix](12-e2e-matrix.md).
