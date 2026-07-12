#!/bin/sh
# e2e: Microsoft's real fabric-cicd Python tool publishes into fabric-emulator,
# authenticated by entra-emulator. Self-contained: builds fabric-emulator from
# this repo, installs entra-emulator + fabric-cicd if missing, runs the driver.
set -eu

DIR="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$DIR/../.." && pwd)"
WORK="${TMPDIR:-/tmp}/fabric-cicd-e2e"
ENTRA_PORT="${ENTRA_PORT:-18443}"
FABRIC_PORT="${FABRIC_PORT:-19443}"
TENANT=11111111-1111-1111-1111-111111111111

rm -rf "$WORK" && mkdir -p "$WORK/data"
trap 'kill $(cat "$WORK"/*.pid 2>/dev/null) 2>/dev/null || true' EXIT INT TERM

# entra-emulator: PATH first, go install otherwise.
ENTRA_BIN="$(command -v entra-emulator || true)"
if [ -z "$ENTRA_BIN" ]; then
  echo "==> installing entra-emulator"
  GOBIN="$WORK" go install github.com/calvinchengx/entra-emulator/cmd/entra-emulator@latest
  ENTRA_BIN="$WORK/entra-emulator"
fi
echo "==> starting entra-emulator on :$ENTRA_PORT"
ORIGIN_MODE=compat PORT="$ENTRA_PORT" DB_PATH="$WORK/entra.sqlite" TLS_CERT_DIR="$WORK/entra-tls" \
  "$ENTRA_BIN" > "$WORK/entra.log" 2>&1 &
echo $! > "$WORK/entra.pid"

echo "==> building + starting fabric-emulator on :$FABRIC_PORT"
go build -C "$REPO" -o "$WORK/fabric-emulator" ./cmd/fabric-emulator
"$WORK/fabric-emulator" -addr ":$FABRIC_PORT" -data-dir "$WORK/data" \
  -entra-issuer "https://localhost:$ENTRA_PORT/$TENANT/v2.0" -entra-tls-insecure \
  > "$WORK/fabric.log" 2>&1 &
echo $! > "$WORK/fabric.pid"

for i in $(seq 1 50); do
  curl -sk "https://localhost:$FABRIC_PORT/health" > /dev/null 2>&1 &&
    curl -sk "https://localhost:$ENTRA_PORT/health" > /dev/null 2>&1 && break
  sleep 0.2
done

echo "==> installing fabric-cicd"
python3 -m venv "$WORK/venv"
"$WORK/venv/bin/pip" install -q fabric-cicd

echo "==> running fabric-cicd against the emulator"
env \
  ENTRA_PORT="$ENTRA_PORT" FABRIC_PORT="$FABRIC_PORT" \
  REQUESTS_CA_BUNDLE="$WORK/data/tls/cert.pem" \
  FABRIC_API_ROOT_URL="https://api.fabric.microsoft.com:$FABRIC_PORT" \
  DEFAULT_API_ROOT_URL="https://api.fabric.microsoft.com:$FABRIC_PORT" \
  FABRIC_CICD_RETRY_DELAY_OVERRIDE_SECONDS=0 \
  ${FABRIC_CICD_DEBUG:+FABRIC_CICD_DEBUG=1} "$WORK/venv/bin/python" -u "$DIR/driver.py"
