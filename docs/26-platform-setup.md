# 26 — Platform setup: Linux, macOS, Windows

Once the prerequisites are in place the workflow is **identical on all three
platforms** — same targets, same output, no per-OS branches to remember:

```bash
make doctor   # is this machine wired up? (run this first)
make up       # start the stack
make status   # is the stack actually usable?
```

Only the setup differs, and only in how you obtain four things:

| Need | Why | Linux | macOS | Windows |
|---|---|---|---|---|
| **POSIX shell** | the `Makefile` recipes and `scripts/*.sh` are `/bin/sh` | built in | built in | Git for Windows (`sh.exe`) |
| **GNU Make** | the target wrappers | `make` package | Xcode Command Line Tools | `ezwinports.make` |
| **Container runtime + Compose v2** | the stack itself | Docker Engine | Docker Desktop / OrbStack / Colima / Rancher | Docker Desktop / Rancher Desktop |
| **Python 3** *(optional)* | `make spark`, `make status-spark`, `make seed` | usually present | Xcode CLT or Homebrew | winget |

Everything below is what `make doctor` checks. Run it before `make up` on any
platform — it names what is missing instead of letting it surface later as a
broken recipe or an unreachable socket.

Give the runtime **at least 8 GB of memory**. The default `make up` includes the
governance profile, where Elasticsearch alone takes a 1 GB heap, plus the SQL
Server and Sail sidecars from the auto-loaded override. `make doctor` warns when
the daemon reports less.

## Linux

Make and Python are usually already there; the runtime is the part worth doing
deliberately.

```bash
sudo apt-get install -y make python3            # Debian/Ubuntu
# sudo dnf install -y make python3              # Fedora/RHEL
```

Install Docker Engine **with the Compose v2 plugin** — `docker-compose` (the old
standalone v1 script) is not enough; these compose files use `depends_on`
conditions and profiles that only v2 understands:

```bash
curl -fsSL https://get.docker.com | sh          # engine + compose plugin
docker compose version                          # must print v2.x or later
```

Then add yourself to the `docker` group, or every command needs `sudo`:

```bash
sudo usermod -aG docker "$USER"
newgrp docker    # or log out and back in — group membership is set at login
```

Skipping that step produces the most common Linux first-run failure, and its
message points at a socket rather than at group membership:

```
permission denied while trying to connect to the Docker daemon socket
```

## macOS

`make` comes with the Xcode Command Line Tools; Python 3 comes with them too:

```bash
xcode-select --install
```

That installs GNU Make **3.81**, which is ancient but sufficient — nothing in
this `Makefile` needs 4.x. If you would rather have a current one,
`brew install make` provides it as `gmake`.

Any of Docker Desktop, [OrbStack](https://orbstack.dev), Rancher Desktop, or
Colima works. Whichever you pick, **raise its memory allocation to 8 GB or
more** — on macOS the runtime is a virtual machine with its own cap, and
Colima in particular defaults to 2 GB, which is not enough for the governance
profile:

```bash
colima start --memory 8            # if using Colima
```

### Apple silicon

Two of the sidecars are x86-only, and they behave differently:

- **SQL Server** (the T-SQL/TDS warehouse backend) has no arm64 image, so it
  runs under Rosetta emulation. It works — the compose healthcheck already
  allows a generous 40 retries and the emulator uses 90-second dial timeouts
  because boot is slow there. Expect the first `make up` to take noticeably
  longer than on x86. Background:
  [16-warehouse-tds.md](16-warehouse-tds.md).
- **`--profile rti`** (Microsoft's Kusto engine) does **not** run on Apple
  silicon at all. The engine's native layer needs AVX2, and Rosetta stops at
  SSE4.2, so the container crashes on boot. It needs a QEMU x86-64 VM with
  `--cpu-type max`; [25-rti-kusto.md](25-rti-kusto.md#running-it-on-apple-silicon)
  has the working Colima recipe. This profile is off by default, so it only
  matters if you ask for it.

Everything else in the stack — the emulators, Sail, OpenMetadata — is
arm64-native.

## Windows

The stack runs natively from PowerShell — no WSL shell, no second checkout
inside a Linux filesystem. Two winget packages, neither needing administrator
rights:

```powershell
winget install Git.Git
winget install ezwinports.make
```

| Package | Why |
|---|---|
| **Git.Git** | supplies `sh.exe` — the shell that runs every recipe — plus the `grep`, `awk`, `cut` and `curl` the scripts call. Installing Git here is *not* about version control; it is how Windows gets a POSIX userland. |
| **ezwinports.make** | GNU Make itself. A standalone build with no MSYS runtime dependency, so it does not fight with Git's. |

`ezwinports.make` is the lightest option. `choco install make` works too but
wants an elevated shell, and the `make` inside WSL only helps if you also move
the checkout and the Docker socket into WSL.

**Open a new terminal afterwards.** winget adds `make` to the user PATH, and an
already-running shell will not see it — the single most common "I installed it
and it still says `make` is not recognized".

Optional, and only for `make spark` / `make status-spark` / `make seed`:

```powershell
winget install Python.Python.3.12
```

### Choosing the container runtime

Docker Desktop and Rancher Desktop both work. Rancher Desktop needs one extra
step, and skipping it produces the least informative error in this document:

```
error during connect: Get "http://%2F%2F.%2Fpipe%2FdockerDesktopLinuxEngine/v1.51/info":
open //./pipe/dockerDesktopLinuxEngine: The system cannot find the file specified.
```

That message names a *pipe*, not a cause. It means the `docker` CLI being
invoked and the daemon actually serving belong to **different vendors**: both
products install a `docker.exe` and both write into the shared `docker context`
list, so if Docker Desktop was ever installed its CLI can win the PATH race
while its context — `desktop-linux` — points at a daemon that is not running.
Rancher Desktop serves the `default` context instead. Select it once and it
persists:

```powershell
docker context ls          # the one marked * is active; find the reachable one
docker context use default
```

`make doctor` reports the active context by name and, when it is unreachable,
lists the alternatives — so you never have to guess which product is serving.

### Which shell?

PowerShell, cmd, or Git Bash — all three work, because `make` switches to
`sh.exe` for the recipe bodies regardless of which shell launched it.

The one thing that does **not** work is running the scripts through cmd or
PowerShell directly (`.\scripts\status.sh`). Go through `make`, or invoke the
shell explicitly: `sh scripts/status.sh`.

### Why `make doctor` exists — three Windows traps

Each of these fails somewhere other than where it originates, which is what
makes them expensive. All three are handled; this records what they were.

**`python3` is a fake.** Windows ships a Microsoft Store *alias stub* named
`python3`. It sits on PATH, so `command -v python3` succeeds and any "is Python
installed?" check passes — then running it exits 49 with a message about
installing from the Store, while a real Python at `python` right beside it is
never consulted. The Makefile and `scripts/status.sh` therefore detect an
interpreter by **executing** each candidate (`python3`, `python`, `py`) and
taking the first that runs. Override with `PY=` if yours lives somewhere
unusual. The symptom: `status.sh` printed `?` for every emulator-state count,
because its JSON parsing silently degraded.

**`/dev/null` is not a path curl understands.** Git Bash's shell understands
`/dev/null`, but `curl.exe` is a native Windows binary that does not — it fails
to open its output file and exits **23 after already printing the status
code**. Written as `curl … || printf '%s' "---"`, and with command substitution
capturing both, every check reported `HTTP 200---` and compared it against
`200`. A perfectly healthy stack was reported as four failed endpoints. The
probe now uses `NUL` on Windows and decides from curl's *output* rather than its
exit status.

**GNU Make falls back to cmd.exe.** When Make cannot find a shell on PATH it
uses `cmd.exe`, which cannot run a single line of these recipes — so the failure
looks like a broken Makefile rather than a missing dependency. The Makefile pins
`SHELL := sh.exe` on Windows, so a missing Git for Windows fails by *naming the
shell*.

### Port 9443 and Rancher Desktop

Rancher Desktop's `steve` API server holds `127.0.0.1:9443` — the same port
fabric-emulator publishes. This is **not** a conflict in practice: the container
binds `0.0.0.0:9443` and wins the forward, so requests reach the emulator.
`make doctor` reports it as a warning rather than a blocker for that reason.

It still matters, because it makes a bare `curl https://localhost:9443/`
ambiguous when the stack is *down* — you get a 200 from `steve` and conclude the
emulator is up. Trust `make status`, which checks container identity and the
emulator's own routes, over a raw probe of the port. Disabling Kubernetes in
Rancher Desktop's settings stops `steve` and frees the port outright.

## Run it

Identical everywhere:

```bash
make doctor      # nothing else is worth trying until this passes
make up
make status
```

`make status` is the real verdict — `make up` returning 0 only means Compose
created the containers. A healthy stack ends with `stack OK`:

```
containers (project: fabric-emulator)
  ok    entra-emulator         healthy
  ok    keyvault-emulator      healthy
  ok    fabric-emulator        healthy
  ok    sail                   healthy
  ok    spark-agent            healthy
  ok    sqlserver              healthy
  …
endpoints
  ok    fabric /health         HTTP 200
  ok    operator portal        HTTP 200
  ok    entra discovery        HTTP 200
```

The portal is then at <https://localhost:9443/> — self-signed TLS, so the
browser warning is expected ([05-tls-and-hosts.md](05-tls-and-hosts.md)) — and
the rest of the [quickstart](01-quickstart.md) applies unchanged.

To prove Spark really computes rather than merely listens: `make spark`.

## Troubleshooting by symptom

| Symptom | Platform | Cause |
|---|---|---|
| `make` is not recognized | Windows | PATH not refreshed — open a new terminal |
| recipes fail with cmd.exe syntax errors | Windows | Git for Windows not installed, so no `sh.exe` |
| `permission denied … docker daemon socket` | Linux | not in the `docker` group; `newgrp docker` |
| `open //./pipe/dockerDesktopLinuxEngine` | Windows | wrong docker context — `docker context use default` |
| `Python was not found` / counts print `?` | Windows | the Store alias stub; install a real Python |
| containers OOM or Elasticsearch dies | macOS, Windows | runtime VM under 8 GB |
| `sqlserver` slow to become healthy | macOS (Apple silicon) | x86 emulation; expected, it does finish |
| `kustainer` crashes on boot | macOS (Apple silicon) | no AVX2 under Rosetta — see [25-rti-kusto.md](25-rti-kusto.md#running-it-on-apple-silicon) |
| `set: Illegal option -` running a script | Linux, macOS | the script was checked out with CRLF; see below |
| `govern-ingest` exits 1 after a `git pull`, everything else healthy | any | its image is built locally, so `docker compose pull` cannot refresh it — rebuild, see below |

### After a pull, rebuild what compose cannot pull

`govern-ingest` declares a `build:` and no `image:`, so it exists only as a
local build and `docker compose pull` skips it. A pull that changes
[`pyproject.toml`](../pyproject.toml) or `uv.lock` therefore leaves it running
an image with the old dependency set, and the failure is quiet — every other
container is healthy and only the one-shot exits non-zero:

```bash
docker compose --profile governance build govern-ingest
make up
```

`sail` and `spark-agent` declare both `image:` and `build:`, so they refresh
from GHCR on a plain pull; only `govern-ingest` has no published image to fall
back on. Details in [22-openmetadata.md](22-openmetadata.md).

### A note on line endings

`scripts/*.sh` must be **LF**. A shell script checked out with CRLF fails at the
shebang — `sh` reads the trailing `\r` as part of the interpreter path, and the
error names a file that plainly exists. Git for Windows sets
`core.autocrlf=true` in its *system* config, so this is the Windows default
rather than a misconfiguration. [`.gitattributes`](../.gitattributes) pins
`*.sh`, `*.py`, `Makefile` and the compose YAML to `eol=lf` so the checkout is
byte-identical on every platform regardless of local Git settings.

## Verified on

Windows 11 with GNU Make 4.4.1 (ezwinports), Git for Windows 2.51, Python 3.12
and Rancher Desktop's dockerd 29.1.3 + Compose v5 — full governance profile,
eleven containers healthy, `make spark` computing on Sail. The Linux path
(dash, GNU coreutils, `python3`) is exercised under WSL Ubuntu 24.04. CI
additionally runs the Go test suite on Linux, macOS and Windows
([10-testing.md](10-testing.md)).
