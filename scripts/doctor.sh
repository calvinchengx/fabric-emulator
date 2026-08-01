#!/bin/sh
# Is this machine able to run `make up` at all?
#
# Separate from status.sh on purpose: status.sh answers "is the stack usable?"
# and assumes the toolchain works. This answers the question that comes before
# it — "is the toolchain and the docker daemon wired up?" — because on Windows
# the failures there are silent and misattributed. Three of them cost real time:
#
#   * `python3` resolves to the Microsoft Store alias stub. It is on PATH, so
#     every `command -v python3` check passes, and it exits 49 when run.
#   * GNU Make falls back to cmd.exe when sh.exe is not on PATH, and every
#     recipe here is POSIX shell.
#   * The `docker` CLI can be one vendor's while the ACTIVE CONTEXT points at
#     another vendor's daemon that is not running (Docker Desktop's CLI first
#     on PATH, Rancher Desktop actually serving). The error is a raw named-pipe
#     failure that names neither.
#
# Exit 0 = ready, 1 = at least one blocker.
set -eu

RC=0
ok()   { printf '  ok    %-22s %s\n' "$1" "$2"; }
warn() { printf '  warn  %-22s %s\n' "$1" "$2"; }
bad()  { printf '  FAIL  %-22s %s\n' "$1" "$2"; RC=1; }

printf 'shell tools\n'
printf '  (recipes and scripts/*.sh are POSIX shell; on Windows these come from Git for Windows)\n'
for t in sh grep awk cut curl docker; do
  p=$(command -v "$t" 2>/dev/null || true)
  if [ -n "$p" ]; then ok "$t" "$p"; else bad "$t" "not on PATH"; fi
done

printf '\npython (scripts/spark_check.py, scripts/govern_ingest.py)\n'
PY="${PY:-}"
if [ -z "$PY" ]; then
  for c in python3 python py; do
    if "$c" -c '' >/dev/null 2>&1; then PY="$c"; break; fi
  done
fi
if [ -n "$PY" ]; then
  ok "$PY" "$("$PY" -c 'import sys; print(sys.version.split()[0], "at", sys.executable)')"
  # Name the trap explicitly when it is present but shadowing a working python.
  if [ "$PY" != "python3" ] && command -v python3 >/dev/null 2>&1; then
    warn "python3" "on PATH but not runnable (Microsoft Store alias stub); using $PY"
  fi
else
  bad "python" "none of python3/python/py runs; make spark and the --spark check will not work"
fi

printf '\ndocker\n'
if ! command -v docker >/dev/null 2>&1; then
  bad "daemon" "no docker CLI"
else
  # Take one line and reject anything that is not a bare context name. A broken
  # `docker` on PATH is a real case — inside WSL without Docker Desktop's
  # integration, the shim prints a multi-line "could not be found in this WSL 2
  # distro" advert on STDOUT, not stderr, so redirecting stderr does not stop it
  # from being captured and pasted into the middle of our own message.
  ctx=$(docker context show 2>/dev/null | head -n 1 | tr -d '\r')
  case "$ctx" in
    ''|*[!A-Za-z0-9_.-]*) ctx=unknown ;;
  esac
  if docker info >/dev/null 2>&1; then
    ok "context" "$ctx"
    ok "daemon" "$(docker info --format '{{.ServerVersion}} ({{.OSType}}/{{.Architecture}}), {{.NCPU}} cpu, {{.MemTotal}} bytes' 2>/dev/null)"
    # The governance profile alone wants ~4 GB (Elasticsearch takes a 1 GB heap);
    # with the default override adding SQL Server and Sail, 8 GB is the floor.
    mem=$(docker info --format '{{.MemTotal}}' 2>/dev/null || printf 0)
    if [ "$mem" -lt 8000000000 ] 2>/dev/null; then
      warn "memory" "$mem bytes; the governance profile + sidecars want ~8 GB"
    fi
  else
    bad "daemon" "context '$ctx' is not reachable"
    printf '        contexts available:\n'
    docker context ls --format '          {{.Name}}  {{.DockerEndpoint}}' 2>/dev/null \
      | grep -E '^ +[A-Za-z0-9_.-]+ ' || printf '          (none — the docker CLI itself is not working)\n'
    printf '        if another runtime (Rancher Desktop, Colima, podman) is serving, select it:\n'
    printf '          docker context use <name>\n'
  fi
  if docker compose version >/dev/null 2>&1; then
    ok "compose" "$(docker compose version --short 2>/dev/null)"
  else
    bad "compose" "docker compose v2 plugin not available"
  fi
fi

# A port already bound is the failure status.sh describes as its "silent
# killer": compose creates the container, the bind fails, and the container
# runs attached to no network while looking healthy. Cheaper to catch here.
#
# Reported as a warning, never a blocker, because "in use" does not reliably
# predict "the bind will fail". Rancher Desktop is the standing example: its
# `steve` API server holds 127.0.0.1:9443 — the same port fabric-emulator
# publishes — and the container still binds AND wins the forward, so requests
# reach the emulator. What is genuinely worth knowing is the ambiguity: if a
# probe answers on one of these ports, confirm you are talking to the emulator
# and not to whatever got there first.
printf '\nhost ports the stack binds\n'
if [ -n "$PY" ]; then
  # `|| true` because of `set -eu` above: this section is advisory, and a python
  # that dies here must not abort the report before its verdict line.
  "$PY" - <<'EOF' || true
import socket
PORTS = [
    (8443,  "entra-emulator"),
    (8444,  "keyvault-emulator"),
    (9443,  "fabric-emulator"),
    (1433,  "fabric warehouse TDS"),
    (50051, "sail (Spark Connect)"),
    (8585,  "openmetadata (governance)"),
]
busy = []
for port, who in PORTS:
    s = socket.socket()
    s.settimeout(0.4)
    taken = s.connect_ex(("127.0.0.1", port)) == 0
    s.close()
    if taken:
        busy.append(port)
        print("  warn  %-22s port %d already answering" % (who, port))
    else:
        print("  ok    %-22s port %d free" % (who, port))
if busy:
    print("        something already listens on %s." % ", ".join(map(str, busy)))
    print("        compose may still bind and win the forward (Rancher Desktop's")
    print("        steve holds 9443 and loses to the container) — but verify with")
    print("        `make status` rather than assuming a 200 came from the emulator.")
EOF
else
  warn "ports" "skipped (needs python)"
fi

printf '\n'
if [ "$RC" = "0" ]; then
  printf 'ready — run: make up\n'
else
  printf 'not ready (see FAIL above)\n'
fi
exit "$RC"
