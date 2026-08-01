#!/bin/sh
# One view of "is the stack actually usable?", because no single existing surface
# answers that. `docker compose ps` knows containers but not whether the emulator
# serves; the operator portal knows emulator state but not whether containers are
# up; and two services (sail, spark-agent) declare no healthcheck, so "running" is
# not evidence they serve.
#
# It also checks the thing that is invisible everywhere else: a container can be
# up AND healthy while attached to NO network, which happens when a port bind
# fails during creation. Docker leaves the container running, compose reuses it
# because it looks healthy, and every peer then fails DNS resolution on its name.
# That failure mode costs an hour if you are reading health status alone.
#
# Exit 0 = everything checked is good, 1 = at least one problem (usable in CI).
#
# `--spark` adds a deep check: open a real Livy session and execute statements.
# Off by default because it costs a few seconds of session startup, and because
# the compose healthchecks already cover liveness.
set -eu

SPARK_DEEP=0
for arg in "$@"; do
  case "$arg" in
    --spark) SPARK_DEEP=1 ;;
    -h|--help) echo "usage: $0 [--spark]   (--spark runs a real Livy computation)"; exit 0 ;;
    *) echo "unknown option: $arg" >&2; exit 2 ;;
  esac
done

# The null device is not spelled the same everywhere. Under Git for Windows the
# SHELL understands /dev/null, but curl.exe is a native Windows binary that does
# not: `-o /dev/null` fails to open its output file and curl exits 23 AFTER
# already printing the status code. That turned every probe below into
# "HTTP 200---" (the code, then the failure fallback appended) and reported a
# healthy stack as broken. NUL is the Windows spelling and creates no file.
NULDEV=/dev/null
case "$(uname -s 2>/dev/null || echo unknown)" in
  MINGW*|MSYS*|CYGWIN*) NULDEV=NUL ;;
esac

PROJECT="${COMPOSE_PROJECT_NAME:-fabric-emulator}"
# One-shot jobs, not services: they are SUPPOSED to run once and exit. Naming
# them explicitly is the only reliable signal — nothing in the container labels
# distinguishes "job still working" from "server that never became ready", and
# reporting a mid-flight job as an unverified service is just wrong.
ONESHOT="om-migrate govern-ingest"
FABRIC="${FABRIC_URL:-https://localhost:9443}"
ENTRA="${ENTRA_URL:-https://localhost:8443}"
OM="${OM_URL:-http://localhost:8585}"
TENANT="${TENANT_ID:-11111111-1111-1111-1111-111111111111}"
RC=0

say() { printf '%s\n' "$*"; }
bad() { RC=1; }

# Portal payloads nest children (capacities embed their workspaces), so
# counting `"id"` occurrences over-reports. Parse the top-level list instead,
# and degrade to "?" rather than lying if no python is available.
#
# Locating an interpreter is not enough to know one exists: on Windows
# `python3` is normally the Microsoft Store ALIAS STUB, which sits on PATH (so
# `command -v python3` succeeds) and then exits 49 telling you to install from
# the Store. Run each candidate instead, and take the first that executes.
PY="${PY:-}"
if [ -z "$PY" ]; then
  for c in python3 python py; do
    if "$c" -c '' >/dev/null 2>&1; then PY="$c"; break; fi
  done
fi
if [ -n "$PY" ]; then HAVE_PY=1; else HAVE_PY=0; fi
count_value() {
  if [ "$HAVE_PY" = "1" ]; then
    printf '%s' "$1" | "$PY" -c 'import sys,json
d = json.load(sys.stdin)
print(len(d.get("value", [])) if isinstance(d, dict) else len(d))' 2>/dev/null || printf '?'
  else
    printf '?'
  fi
}

# HTTP probe: prints the status code, or "---" when unreachable.
#
# curl's exit status is deliberately NOT chained with `||` here. It prints the
# code on stdout and then can still exit non-zero for reasons unrelated to the
# HTTP result (see NULDEV above), and inside `$(...)` a fallback `printf` would
# be APPENDED to the code rather than replacing it. Decide from the value.
code() {
  c=$(curl -sk -o "$NULDEV" -w '%{http_code}' --max-time 5 "$1" 2>/dev/null)
  case "$c" in
    ''|000|*[!0-9]*) printf '%s' "---" ;;
    *)               printf '%s' "$c" ;;
  esac
}

check_http() { # url label expected
  c=$(code "$1")
  if [ "$c" = "$3" ]; then
    printf '  ok    %-22s %s\n' "$2" "HTTP $c"
  else
    printf '  FAIL  %-22s %s (want %s)\n' "$2" "HTTP $c" "$3"; bad
  fi
}

say "containers (project: $PROJECT)"
ids=$(docker ps -aq --filter "label=com.docker.compose.project=$PROJECT" 2>/dev/null || true)
if [ -z "$ids" ]; then
  say "  FAIL  no containers. Start with: docker compose up -d"; bad
else
  for id in $ids; do
    # One inspect per container, tab-separated, so the shell does no JSON parsing.
    line=$(docker inspect "$id" --format \
'{{index .Config.Labels "com.docker.compose.service"}}	{{.State.Status}}	{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}	{{len .NetworkSettings.Networks}}	{{if .State.ExitCode}}{{.State.ExitCode}}{{else}}0{{end}}')
    svc=$(printf '%s' "$line" | cut -f1)
    status=$(printf '%s' "$line" | cut -f2)
    health=$(printf '%s' "$line" | cut -f3)
    nets=$(printf '%s' "$line" | cut -f4)
    exitc=$(printf '%s' "$line" | cut -f5)

    note=""
    mark="ok  "
    case " $ONESHOT " in *" $svc "*) oneshot=1 ;; *) oneshot=0 ;; esac
    case "$status" in
      running)
        case "$health" in
          healthy)   note="healthy" ;;
          starting)  note="health starting"; mark="warn" ;;
          unhealthy) note="UNHEALTHY"; mark="FAIL"; bad ;;
          none)
            if [ "$oneshot" = "1" ]; then
              note="running (one-shot job still working)"
            else
              note="running (no healthcheck, serving unverified)"; mark="warn"
            fi
            ;;
        esac
        # The silent killer: up, but on no network, so its DNS name does not exist.
        if [ "$nets" = "0" ]; then
          note="$note; ON NO NETWORK (peers cannot resolve it) -> docker compose up -d --force-recreate $svc"
          mark="FAIL"; bad
        fi
        ;;
      exited)
        # One-shot jobs (migrations, ingest) exit 0 by design; only non-zero is a problem.
        if [ "$exitc" = "0" ]; then
          note="exited 0 (one-shot, done)"
        else
          # Show how long ago: docker renders it in words, and a failure from an
          # hour ago reads as current otherwise.
          when=$(docker ps -a --filter "id=$id" --format '{{.Status}}' 2>/dev/null || printf '')
          note="${when:-exited $exitc}"
          # Only a one-shot can be stale this way: `compose run --rm` removes its
          # own container, leaving the older `up` one behind with its old exit.
          if [ "$oneshot" = "1" ]; then
            note="$note (stale if since re-run via \`compose run --rm\`)"
          fi
          mark="FAIL"; bad
        fi
        ;;
      restarting) note="restarting (crash loop)"; mark="FAIL"; bad ;;
      *)          note="$status"; mark="warn" ;;
    esac
    printf '  %-5s %-22s %s\n' "$mark" "$svc" "$note"
  done
fi

say ""
say "endpoints"
check_http "$FABRIC/health" "fabric /health" 200
check_http "$FABRIC/" "operator portal" 200
check_http "$ENTRA/$TENANT/v2.0/.well-known/openid-configuration" "entra discovery" 200
check_http "$OM/" "openmetadata UI" 200

say ""
say "emulator state (via the portal API the SPA reads)"
for ep in workspaces capacities jobs operations connections shortcuts; do
  body=$(curl -sk --max-time 5 "$FABRIC/_emulator/portal/$ep" 2>/dev/null || printf '')
  if [ -z "$body" ]; then
    printf '  FAIL  %-14s unreachable\n' "$ep"; bad
  else
    printf '  ok    %-14s %s\n' "$ep" "$(count_value "$body")"
  fi
done

# Seeded state is in-memory unless FABRIC_DATA_DIR is set, so an empty workspace
# list after a restart is expected, not a bug. Say so instead of looking broken.
ws=$(count_value "$(curl -sk --max-time 5 "$FABRIC/_emulator/portal/workspaces" 2>/dev/null || printf '')")
if [ "$ws" = "0" ]; then
  say ""
  say "  note: no workspaces yet. State is in-memory unless FABRIC_DATA_DIR is set,"
  say "        so it resets on restart. govern-ingest will seed a demo workspace"
  say "        (GOVERN_SEED_DEMO=0 to opt out); or create your own, quickstart step 3."
fi

if [ "$SPARK_DEEP" = "1" ]; then
  say ""
  say "spark (deep: a real Livy session executing statements)"
  if [ "$HAVE_PY" = "1" ]; then
    if "$PY" "$(dirname "$0")/spark_check.py"; then :; else bad; fi
  else
    say "  skip  no working python to run scripts/spark_check.py"
  fi
fi

say ""
if [ "$RC" = "0" ]; then say "stack OK"; else say "stack has problems (see FAIL above)"; fi
exit "$RC"
