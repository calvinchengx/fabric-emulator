#!/bin/sh
# Why `docker compose up` just failed to bind, in the terms you need to fix it.
#
# Compose says "Bind for 0.0.0.0:8443 failed: port is already allocated" and
# stops there. It names the port and not the holder, which is the one fact that
# decides what to do next — and it leaves the services that DID start running,
# so the stack is half up. A health probe against one of these ports can then
# answer from whatever got there first, and everything downstream looks like it
# is talking to this stack when it is not. That has cost real debugging time
# here, twice: once on 18443 against another project's entra, once on 9443.
#
# This runs only AFTER a failed `up` (see the Makefile). It is not a pre-flight
# check on purpose: scripts/doctor.sh already reports busy ports as a warning,
# and deliberately not as a blocker, because "in use" does not reliably predict
# "the bind will fail" — Rancher Desktop's steve API server holds 127.0.0.1:9443
# and the container still binds and still wins the forward. Blocking on that
# would refuse to start a stack that works.
set -u

printf '\n--- port conflict ---\n'
printf 'compose could not publish a port. Who holds the ones this stack needs:\n\n'

# The published ports across docker-compose.yml and its overlays.
PORTS='8443 8444 9443 1433 50051 8585'
found=0

for port in $PORTS; do
  # A container publishing this host port, if any. `docker ps` is the direct
  # answer for the common case (another compose project on the same machine),
  # and names the project so you know whose it is.
  holder=$(docker ps --format '{{.Names}}\t{{.Ports}}' 2>/dev/null \
           | grep -E "(0\.0\.0\.0|\[::\]):${port}->" | cut -f1 | head -1)
  if [ -n "$holder" ]; then
    project=$(docker inspect "$holder" \
              --format '{{index .Config.Labels "com.docker.compose.project"}}' 2>/dev/null)
    if [ -n "$project" ]; then
      printf '  %-6s held by container %s  (compose project: %s)\n' "$port" "$holder" "$project"
    else
      printf '  %-6s held by container %s\n' "$port" "$holder"
    fi
    found=1
    continue
  fi
  # Not a container. Something on the host, then — lsof if it is available.
  if command -v lsof >/dev/null 2>&1; then
    proc=$(lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | awk 'NR==2 {print $1" (pid "$2")"}')
    if [ -n "$proc" ]; then
      printf '  %-6s held by host process %s\n' "$port" "$proc"
      found=1
    fi
  fi
done

[ "$found" -eq 0 ] && printf '  (nothing is holding these ports now — the conflict may have cleared)\n'

cat <<'EOF'

What to do:
  * another compose project        stop it, or run this stack on other ports
  * a host process you need        run this stack on other ports
  * leftovers from an earlier run  make clean

To run on other ports, remap ONLY the host side in an overlay. Compose appends
`ports` rather than replacing them, so a plain mapping adds a second binding and
still collides — use the explicit merge control:

    # ports-isolated.yml
    services:
      entra-emulator:  { ports: !override ["18543:8443"] }
      fabric-emulator: { ports: !override ["19543:9443", "11533:1433"] }

    docker compose -p myrun -f docker-compose.yml -f docker-compose.override.yml \
      -f ports-isolated.yml up -d

Container ports and every service-to-service URL stay as they are, so nothing
about the stack's behaviour changes — only where it is reachable from the host.

THE STACK IS NOW HALF UP. Services that bound before the failure are still
running. `make down` clears them; `make status` says what is actually usable.
Do not trust a health probe on the ports above until this is resolved: it can
answer from whichever service got there first.
EOF
