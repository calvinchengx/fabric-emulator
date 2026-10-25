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
#     failure that names neither -- so an unreachable context reports the
#     ENDPOINT it could not reach, which is the one string the user can match
#     against the error they saw, and the alternatives are PROBED rather than
#     listed: a context that is reachable but serving windows containers is no
#     answer either.
#   * The same named-pipe failure is what Docker Desktop simply NOT RUNNING
#     looks like, because the pipe exists only while it runs. Issue #650 is
#     that one, reported as a `docker pull` fault: "open
#     //./pipe/dockerDesktopLinuxEngine: The system cannot find the file
#     specified". So the advice for an unreachable context cannot be only
#     "select another one" -- with one context installed there is none to
#     select, and the answer is to start the runtime.
#   * Docker Desktop in WINDOWS CONTAINERS mode answers `docker info` happily,
#     so every check here passed and the report said "ready". Every image in
#     this stack is linux, so the failure landed two commands later as "no
#     matching manifest for windows/amd64 in the manifest list entries" -- a
#     message about manifests, from a daemon that is working exactly as
#     configured. OSType is therefore a blocker, not a detail.
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
    # Every image this stack pulls is linux. A windows-containers daemon is
    # reachable, versioned and healthy -- it just cannot run any of them.
    ostype=$(docker info --format '{{.OSType}}' 2>/dev/null | tr -d '\r')
    if [ -n "$ostype" ] && [ "$ostype" != "linux" ]; then
      bad "engine" "serving $ostype containers; every image here is linux"
      printf '        switch the engine over -- Docker Desktop tray icon ->\n'
      printf '        "Switch to Linux containers...", or from a shell:\n'
      printf '          & "$Env:ProgramFiles\\Docker\\Docker\\DockerCli.exe" -SwitchLinuxEngine\n'
    fi
    # The governance profile alone wants ~4 GB (Elasticsearch takes a 1 GB heap);
    # with the default override adding SQL Server and Sail, 8 GB is the floor.
    mem=$(docker info --format '{{.MemTotal}}' 2>/dev/null || printf 0)
    if [ "$mem" -lt 8000000000 ] 2>/dev/null; then
      warn "memory" "$mem bytes; the governance profile + sidecars want ~8 GB"
    fi
  else
    # One `context ls` answers both questions below: which endpoint the active
    # context could not reach, and what else is installed. Rows are filtered to
    # a bare name and a scheme-bearing endpoint for the reason the context name
    # is sanitised above -- a `docker` that cannot work prints prose here, and
    # prose must not be mistaken for a context to probe.
    rows=$(docker context ls --format '{{.Name}} {{.DockerEndpoint}}' 2>/dev/null | tr -d '\r' \
      | awk '$1 ~ /^[A-Za-z0-9_.-]+$/ && $2 ~ /:\/\// { print $1, $2 }')
    endpoint=$(printf '%s\n' "$rows" | awk -v c="$ctx" '$1 == c { print $2; exit }')
    # The endpoint belongs in the verdict line, not only in the advice: it is
    # the one string the reader can match against the error their own `docker`
    # command printed, which names the pipe and nothing else.
    if [ -n "$endpoint" ]; then
      bad "daemon" "context '$ctx' ($endpoint) is not reachable"
    else
      bad "daemon" "context '$ctx' is not reachable"
    fi
    # Which of the two causes this is depends on the endpoint. Docker Desktop's
    # Linux-engine pipe does not exist while Docker Desktop is stopped, so the
    # same text that means "wrong vendor selected" also means "not started" --
    # and the latter is the common one. Name it first.
    case "$endpoint" in
      *dockerDesktopLinuxEngine*)
        printf '        %s is Docker Desktop'"'"'s Linux engine, and that pipe\n' "$endpoint"
        printf '        exists only while Docker Desktop is RUNNING. Start it and wait for the\n'
        printf '        whale icon to stop animating; "The system cannot find the file\n'
        printf '        specified" on this pipe is a stopped engine, not a broken install.\n'
        ;;
      *)
        printf '        start the runtime that serves it, or if it is gone, select one below.\n'
        ;;
    esac
    # The alternatives, each PROBED rather than merely named. "Another context
    # exists" is not advice: it may be as stopped as this one, and one that
    # answers while serving windows containers cannot run these images either.
    # The verdict rides on the row, so the list still reads as a list.
    peers=$(printf '%s\n' "$rows" | awk -v c="$ctx" '$1 != c { print $1 }')
    if [ -z "$rows" ]; then
      printf '        contexts available:\n'
      printf '          (none — the docker CLI itself is not working)\n'
    elif [ -z "$peers" ]; then
      # Issue #650's machine. "Select another context" was the advice here, and
      # there is no other context: the runtime is what has to start.
      printf '        it is also the only context configured, so there is nothing else to\n'
      printf '        select -- the runtime itself has to be started (or installed).\n'
    else
      printf '        contexts available:\n'
      serves=''
      for c in $peers; do
        ep=$(printf '%s\n' "$rows" | awk -v c="$c" '$1 == c { print $2; exit }')
        cos=$(docker --context "$c" info --format '{{.OSType}}' 2>/dev/null | tr -d '\r')
        case "$cos" in
          linux)
            printf '          %s  %s  (answers, linux)\n' "$c" "$ep"
            [ -n "$serves" ] || serves=$c
            ;;
          '') printf '          %s  %s  (also unreachable)\n' "$c" "$ep" ;;
          *)  printf '          %s  %s  (answers, but serves %s containers)\n' "$c" "$ep" "$cos" ;;
        esac
      done
      if [ -n "$serves" ]; then
        printf '        %s answers and serves linux containers; select it:\n' "$serves"
        printf '          docker context use %s\n' "$serves"
      else
        # Nothing probed clean. The vendor-clash advice still stands as a hint,
        # because a context can need env the probe did not supply -- but it is
        # phrased as the guess it now is, rather than as the fix.
        printf '        if another runtime (Rancher Desktop, Colima, podman) is serving, select it:\n'
        printf '          docker context use <name>\n'
      fi
    fi
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
