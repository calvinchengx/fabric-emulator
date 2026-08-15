# 52 — Full DAX oracle: msmdsrv on three hosts

**Status: attach, pump, and model publish landed.** The bounded Go evaluator
stays the default on every OS. A real Analysis Services process
(`msmdsrv.exe`) is an **optional oracle**, reached through `FABRIC_DAX_URL`,
never a compose default, and never started by `make up` / `make up-lite`.
This is the host map for that oracle — the thing
[33-pbix-tooling.md](33-pbix-tooling.md) proved on `windows-latest` (Desktop
answers bit-identically) and the thing Wine / OrbStack / Rancher Desktop on
macOS cannot host.

Same move as Eventhouse ([25-rti-kusto.md](25-rti-kusto.md)) and Eventstream
([51-eventstream-kafka.md](51-eventstream-kafka.md)): terminate Fabric's
contract ourselves, relay the compute to a real engine. The difference is
the engine. Microsoft ported the SQL **database** engine to Linux; they did
not port Analysis Services. There is no `mcr.microsoft.com/.../msmdsrv`.
So the sidecar is a **Windows guest**, and the guest is started differently
on each OS.

## What is in and what is out

| In | Out |
|---|---|
| Query relay: `executeQueries` (and the portal runner) POST DAX to a pump in front of `msmdsrv` when `FABRIC_DAX_URL` is set | Putting `dockur/windows` on `docker compose up` or `--profile dax` for Mac users |
| Three recipes for a **machine you own** that actually has a Windows kernel | Wine, Moonshine, Game Porting Toolkit, OrbStack, Rancher Desktop on macOS |
| Growing the Go subset against Desktop goldens so every-push CI stays on the in-process engine | Claiming "full DAX" because a VM can boot, or because the host recipes exist |
| Honest 502 when the URL is set and the pump is down | Silent fallback to the Go subset (that would hide a dead oracle) |
| The CI oracle already on `windows-latest` (`e2e/pbix-desktop`) | UTM on `macos-latest`, or `dockur/windows` on `ubuntu-latest`, as a required CI job |

Wine maps user-mode Win32 onto Darwin. `msmdsrv` is a Windows service
(SCM, COM, named pipes, SSPI, VertiPaq). SQL Server on a Mac is the official
**Linux** image, not `wine sqlservr.exe`. Analysis Services is still listed
unsupported on Linux through SQL Server 2025. That is why this page is a
host map and not a Wine bottle.

## The three hosts

These rows are **developer (or self-hosted) machines**, not GitHub-hosted
runner labels. `macos-latest` is not "the macOS row." `ubuntu-latest` is
not "the Linux row." See [Not GitHub-hosted runners](#not-github-hosted-runners).

| Host (a machine you own) | How Windows runs | How you start `msmdsrv` | What the Mac/Linux emulator sets |
|---|---|---|---|
| **macOS** | [UTM](https://github.com/utmapp/UTM) (or Parallels) on the **host**, not inside OrbStack | Install Windows in the guest, then Power BI Desktop or SSAS Developer. Desktop hosts `msmdsrv` as a child; the port is in `%LOCALAPPDATA%\Microsoft\Power BI Desktop*\*\Data\msmdsrv.port.txt` — the same file `e2e/pbix-desktop/desktop.ps1` already polls. Then `pwsh e2e/msmdsrv/start.ps1` | `FABRIC_DAX_URL=http://<guest-ip>:8080` from the Mac |
| **Linux** | Docker **Engine on the metal** (or a VM that already has KVM) + [`dockur/windows`](https://github.com/dockur/windows) with `/dev/kvm` | Same guest install as Windows. Compose file: [`e2e/msmdsrv/docker-compose.yml`](../e2e/msmdsrv/docker-compose.yml). `make dax-linux` refuses unless `uname` is Linux **and** `/dev/kvm` exists. Pump still starts inside the guest | `FABRIC_DAX_URL=http://127.0.0.1:8080` (published port) |
| **Windows** | Nothing. The kernel is already there | Desktop (the path `e2e/pbix-desktop` already runs in CI) plus `FABRIC_DAX_URL` (#232). Headless `msmdsrv` listens with a shoestring parent PID ([33](33-pbix-tooling.md) Phase 0c); `ROW` needs a table, so it is not a ready oracle. Then `pwsh e2e/msmdsrv/start.ps1` | `FABRIC_DAX_URL=http://127.0.0.1:8080` |

`dockur/windows` is QEMU in a Linux container. It needs `/dev/kvm` passed
in. Their own requirements are Linux+KVM or Docker Desktop on **Windows 11**
with nested virt. They call out Docker Desktop on macOS as unsupported.
OrbStack and Rancher Desktop on a Mac have the same hole: the Linux VM does
not expose `/dev/kvm`. `KVM=N` (TCG) is software emulation of Windows inside
Linux inside macOS — the project itself says that is a major performance
loss. Do not use it.

On Apple Silicon, a UTM guest should be **Windows 11 ARM**. x64 Desktop
inside ARM Windows is emulation; prefer the ARM Desktop build when it is
the thing that hosts `msmdsrv`.

## Not GitHub-hosted runners

The host table is how a person reaches `msmdsrv` without waiting for
Monday's Windows job. It is **not** how every-push CI gets full DAX.

GitHub-hosted `macos-latest` and `ubuntu-latest` are already VMs.
Windows inside them is nested virtualization.

- **`macos-latest` + UTM.** GitHub does not support nested virtualization
  on macOS images. UTM is a GUI hypervisor. There is no Hypervisor.framework
  for a Windows 11 ARM guest on that runner.
- **`ubuntu-latest` + `dockur/windows`.** `/dev/kvm` exists on x64 Linux
  runners for the Android emulator (small, cached images). GitHub still
  calls nested VMs experimental and unsupported. `dockur/windows` is a
  full Windows 11 install (our compose asks for 8 GB RAM; their docs want
  ≥32 GB disk). First boot *installs Windows*. That is not a PR job, and
  caching a Windows VHD across jobs is a size and licence question. ARM
  Ubuntu runners do not get KVM.

The CI witness is **`e2e/pbix-desktop` on `windows-latest`**. That kernel
*is* Windows. Desktop hosts `msmdsrv` as a child. No nested guest.

Every-push tests on ubuntu/mac run the **bounded Go subset** against
Desktop goldens. They do not boot Windows. Empty `FABRIC_DAX_URL` is
what those jobs have, and what they test. A self-hosted runner that
really is Linux+KVM or a Mac with UTM on the metal may use the recipes
above; that is a human's machine, not `runs-on: ubuntu-latest`.

## The attach point

```text
client  --executeQueries-->  fabric-emulator
                                |  FABRIC_DAX_URL empty → internal/semanticmodel
                                |  FABRIC_DAX_URL set   → POST {url}/v1/deploy
                                |                         POST {url}/v1/dax
                                v
                         pump (AMO + ADOMD.NET)
                                |
                                v
                         msmdsrv.exe
```

`msmdsrv` does not speak `executeQueries` REST. The pump is a thin HTTP
front (same job as Microsoft's own
[azure-analysis-services-http-sample](https://github.com/microsoft/azure-analysis-services-http-sample)):
ADOMD to `localhost:<port>`, JSON back. The emulator still does Power BI
audience auth, workspace RBAC, and model-id resolution. Only the EVALUATE
is forwarded.

Pump contract:

```http
POST {FABRIC_DAX_URL}/v1/deploy
{"tmsl":{"createOrReplace":{"object":{"database":"RetailAnalysis"},"database":{…}}}}

POST {FABRIC_DAX_URL}/v1/dax
{"query":"EVALUATE SUMMARIZECOLUMNS(...)","catalog":"RetailAnalysis"}
```

```json
{"rows":[{"Customer[Country]":"US","[Revenue]":101.72}]}
```

Deploy runs once per item definition (sha256 of the TMSL). Import rows from
the item's `data.json` become `DATATABLE` calculated partitions — VertiPaq
will not read our JSON, and [pbix-mcp](https://github.com/d0nk3yhm/pbix-mcp)
is not a runtime dependency ([33](33-pbix-tooling.md)). Direct Lake tables
are refused rather than dropped.

Desktop's workspace instance often **rejects** `CreateOrReplace` of a new
database (it already has the open `.pbix`). The pump returns 409
`DAXDeployRejected`; the emulator then queries whatever catalog is loaded —
the Phase 1 hand-open path still works. SSAS Developer / headless `msmdsrv`
accepts the publish.

A 4xx from the pump is a `DAXQueryError` (400). An unreachable pump is
`DAXEngineUnreachable` (502). The Go subset is **not** consulted when the
URL is set — a dead oracle must be loud.

Empty `FABRIC_DAX_URL` (the default) keeps today's evaluator. No 501 on
the query path: the subset is a real engine for the rows it pins.
GitHub-hosted ubuntu/mac jobs never start `msmdsrv`; they test this
default. See [Not GitHub-hosted runners](#not-github-hosted-runners).

## Running the pump

The pump is [`e2e/msmdsrv/pump`](../e2e/msmdsrv/pump) — a `net8.0` Kestrel
front that opens a new ADOMD connection per request. It must run **inside**
the Windows guest. ADOMD's bare `localhost:<port>` form is Windows-only on
.NET Core, and that is the form a loopback `msmdsrv` answers. Same
`Microsoft.AnalysisServices.AdomdClient.NetCore.retail.amd64` **19.***
package as `e2e/pbix-desktop/probe` (x64-emulated on ARM Windows).

On the guest, start the pump. SSAS / headless `msmdsrv` needs only a
listening engine (`MSMDSRV_PORT` or `MSMDSRV_DATA_SOURCE`). Desktop still
needs a process hosting `msmdsrv` — open any `.pbix` so the port file
exists; the emulator then tries `CreateOrReplace` and falls back to the
open catalog if Desktop refuses.

```powershell
pwsh e2e/msmdsrv/start.ps1
```

`GET /health` opens ADOMD and returns `{"ok":true,"port":"…"}`. Allow inbound
TCP 8080 on the guest firewall if the emulator is on another host.

| Env | Default | Meaning |
|---|---|---|
| `MSMDSRV_PUMP_ADDR` | `http://0.0.0.0:8080` | Kestrel listen URL |
| `MSMDSRV_PORT` | *(Desktop `msmdsrv.port.txt`)* | Loopback port. Unset → re-read the newest Desktop port file per request (Desktop can restart and move it) |
| `MSMDSRV_CATALOG` | *(empty)* | Optional `Initial Catalog=` |
| `MSMDSRV_DATA_SOURCE` | *(empty)* | Full ADOMD connection string; skips port discovery (SSAS named instance) |

On the emulator host:

```bash
export FABRIC_DAX_URL=http://<guest-ip>:8080   # UTM / Parallels
export FABRIC_DAX_URL=http://127.0.0.1:8080    # dockur published port, or native Windows
```

## Phases

### Phase 1 — attach and relay (landed)

- `FABRIC_DAX_URL` on the config / server / `executeQueries` path.
- Unit tests: empty URL uses the Go evaluator; set URL forwards and does
  not fall back; unreachable pump is 502.
- Host recipes on this page. `make dax-linux` is a guard, not a family
  profile.
- Pump: [`e2e/msmdsrv/pump`](../e2e/msmdsrv/pump) +
  [`e2e/msmdsrv/start.ps1`](../e2e/msmdsrv/start.ps1), run **inside** the
  Windows guest. Compose publishes 8080; it does not start the pump.

### Phase 2 — publish the model (landed)

`executeQueries` POSTs `CreateOrReplace` TMSL to `/v1/deploy` before the
first `/v1/dax`. The pump runs it through AMO `Server.Execute`. Desktop
that will not create a database returns 409 and the query still goes to
the open catalog. Opening a `.pbix` by hand remains valid; it is no longer
required on a host that accepts TMSL (SSAS Developer, later headless
`msmdsrv`).

DATATABLE columns must set `sourceColumn` (the BIM `sourceColumn`, or the
column name). Desktop rejects the omit. The pump also maps that refusal —
and empty-partition errors — to 409 so a still-open `.pbix` stays queryable.
If the named catalog is missing on Desktop's workspace instance, `/v1/dax`
retries without `Initial Catalog=` and hits the open file.

### Phase 3 — grow the Go subset against the oracle

Unchanged from [33](33-pbix-tooling.md): every new function is one Desktop
agreed about. A developer VM (UTM / `dockur` on metal) is how a Mac or
Linux laptop reaches that oracle without waiting for Monday's
`windows-latest` job. Every-push CI on ubuntu/mac does **not** boot that
VM; it replays the goldens against Go. That is what keeps those jobs
honest: they test the engine those runners actually have.

Pins so far, in
[`e2e/semantic-model/fixtures/desktop_goldens.json`](../e2e/semantic-model/fixtures/desktop_goldens.json),
replayed by `TestDesktopFunctionGoldens`: `ACOS`, then `ABS` (BLANK stays
BLANK), then `ROUND` (half away from zero; BLANK number stays BLANK,
BLANK digits count as 0), then `LOG` / `LOG10` (default `LOG` base is 10;
BLANK/`<=0` and `LOG` base `1`/`<=0` error), then `SQRT` (BLANK stays BLANK; negative
errors), then `MOD` (`n - d * INT(n/d)`; BLANK n is BLANK; d = 0 errors), then `SIGN` (BLANK stays BLANK; `SIGN(0)` is 0), then `ASIN` / `ATAN` (BLANK stays BLANK; `ASIN` outside `[-1, 1]` errors), then `PI` / `SIN` / `COS` / `TAN` (`COS(BLANK())` is 1; `SIN`/`TAN` BLANK stays BLANK; `TAN` of a right angle errors), then `DEGREES` / `RADIANS` (BLANK stays BLANK), then `DATE` / `YEAR` / `MONTH` / `DAY` (two-digit years 0–30 → 2000s, 31–99 → 1900s; month/day overflow; day `<=0` errors; `YEAR(BLANK())` is BLANK). Captured on a UTM Windows 11 ARM guest + Desktop.
Clone that guest only while it is **stopped** (`utmctl clone` refuses a
running VM). Do not treat the live oracle as disposable.

### Phase 4 — XMLA write-through (only if demanded)

`executeQueries` is the tractable surface. XMLA already terminates on the
Go evaluator. Relaying XMLA Execute to `msmdsrv` is a separate cost and
is not a Phase 1 blocker.

## What this does not change

- Parity row **Power BI — DAX beyond the bounded subset** stays 🟡 until
  the pump step on `e2e/pbix-desktop` (`windows-latest`) has been seen
  green. That job already opened Desktop; it now also starts
  `e2e/msmdsrv` and POSTs `/v1/dax`. Weekly, `continue-on-error`,
  Windows-only. It is not replaced by UTM on `macos-latest` or
  `dockur/windows` on `ubuntu-latest`.
- `make up` / OrbStack / Rancher Desktop on a Mac are unchanged.
- No Microsoft OSS engine was found to vendor
  ([tmdl-parser](https://github.com/microsoft/tmdl-parser) is an empty
  stub; SemPy / TOM / ADOMD are clients). A `tabular-emulator` remains a
  clean-room write if we want full DAX without a Windows guest.

## Licence

Same caveat as [33](33-pbix-tooling.md) Phase 0b: Microsoft documents
`-quiet ACCEPT_EULA=1` for Desktop install. Automated *use* of Desktop or
SSAS in CI is a human decision. This page is the host map, not that
reading.

## Cost, measured where we have numbers

| Path | What we know |
|---|---|
| `e2e/pbix-desktop` on `windows-latest` | ~7–8 min, 698 MB Desktop download, bit-identical on the fixture query (5/5). Same job now starts the DAX pump and POSTs `/v1/dax` |
| UTM on Apple Silicon | Windows guest: budget 8–16 GB RAM, minutes to boot, plus Desktop inside it |
| `dockur/windows` on Linux+KVM | Image + Windows install disk (their docs: ≥32 GB free, 2–4 GB RAM for the guest). First boot installs Windows; later boots reuse `/storage` |
| Wine / GPTK / OrbStack | Not a path. Do not budget it |
| GitHub `macos-latest` + UTM | Not a path. Nested virt unsupported on those images |
| GitHub `ubuntu-latest` + `dockur/windows` | Not a PR job. Nested Windows install is the wrong size and GitHub does not support it |
