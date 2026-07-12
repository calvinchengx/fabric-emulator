#!/bin/sh
# Container smoke test: builds the production image and runs it exactly as a
# user would (default config, a persistent volume), then proves the server
# started AND actually wrote its state to the volume. Catches the class of bug
# where the distroless nonroot user can't open its SQLite DB under /data —
# which shipped untested because nothing in CI ran the container.
set -eu

IMG="${IMG:-fabric-emulator-smoke}"
VOL="fabric-emu-smoke-$$"
CID=""

cleanup() {
	[ -n "$CID" ] && docker rm -f "$CID" >/dev/null 2>&1 || true
	docker volume rm "$VOL" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

echo "==> building the production image"
docker build -q -t "$IMG" "$(dirname "$0")/.." >/dev/null

echo "==> running it with a persistent volume (default config)"
docker volume create "$VOL" >/dev/null
CID=$(docker run -d -v "$VOL":/data \
	-e FABRIC_ENTRA_ISSUER="https://entra.example/11111111-1111-1111-1111-111111111111/v2.0" \
	"$IMG")

echo "==> waiting for health (server opened its DB and is serving)"
ok=""
i=0
while [ "$i" -lt 30 ]; do
	if docker exec "$CID" /usr/local/bin/fabric-emulator healthcheck >/dev/null 2>&1; then
		ok=1
		break
	fi
	# A crash (e.g. can't open DB) exits the container — fail fast.
	if [ "$(docker inspect -f '{{.State.Running}}' "$CID" 2>/dev/null)" != "true" ]; then
		echo "container exited before becoming healthy:"
		docker logs "$CID"
		exit 1
	fi
	i=$((i + 1))
	sleep 1
done
[ "$ok" = 1 ] || { echo "container never became healthy"; docker logs "$CID"; exit 1; }
echo "    healthy"

echo "==> asserting state was persisted to the volume"
listing=$(docker run --rm -v "$VOL":/data busybox sh -c 'ls -1 /data /data/tls 2>/dev/null')
echo "$listing" | grep -q "fabric-emulator.db" || { echo "no DB in the volume:"; echo "$listing"; exit 1; }
echo "$listing" | grep -q "cert.pem" || { echo "no persisted TLS cert in the volume:"; echo "$listing"; exit 1; }
echo "    fabric-emulator.db + tls/cert.pem present"

echo "DOCKER SMOKE: PASS"
