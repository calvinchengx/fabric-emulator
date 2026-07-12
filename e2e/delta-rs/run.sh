#!/bin/sh
# e2e A1: real delta-rs writes/reads Delta tables through the emulator's
# OneLake Blob surface with entra-minted Storage tokens.
set -eu

DIR="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$DIR/../.." && pwd)"
WORK="${TMPDIR:-/tmp}/delta-rs-e2e"
ENTRA_PORT="${ENTRA_PORT:-18443}"
FABRIC_PORT="${FABRIC_PORT:-19080}"
TENANT=11111111-1111-1111-1111-111111111111

rm -rf "$WORK" && mkdir -p "$WORK/data"
trap 'kill $(cat "$WORK"/*.pid 2>/dev/null) 2>/dev/null || true' EXIT INT TERM

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

echo "==> building + starting fabric-emulator on :$FABRIC_PORT (plain HTTP for object_store)"
go build -C "$REPO" -o "$WORK/fabric-emulator" ./cmd/fabric-emulator
"$WORK/fabric-emulator" -addr "127.0.0.1:$FABRIC_PORT" -data-dir "$WORK/data" -disable-tls \
  -entra-issuer "https://localhost:$ENTRA_PORT/$TENANT/v2.0" -entra-tls-insecure \
  > "$WORK/fabric.log" 2>&1 &
echo $! > "$WORK/fabric.pid"

for i in $(seq 1 50); do
  curl -s "http://127.0.0.1:$FABRIC_PORT/health" > /dev/null 2>&1 &&
    curl -sk "https://localhost:$ENTRA_PORT/health" > /dev/null 2>&1 && break
  sleep 0.2
done

echo "==> installing deltalake"
python3 -m venv "$WORK/venv"
"$WORK/venv/bin/pip" install -q deltalake pyarrow

echo "==> running delta-rs against the emulator"
ENTRA_PORT="$ENTRA_PORT" FABRIC_PORT="$FABRIC_PORT" \
  "$WORK/venv/bin/python" -u "$DIR/driver.py"
