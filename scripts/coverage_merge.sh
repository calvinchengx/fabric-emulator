#!/usr/bin/env bash
# Merge unit and e2e coverage into one profile, and report the total.
#
# The Go toolchain can do this without a third-party merger, but only in its
# BINARY format: `go test -coverprofile` emits text, which cannot be merged, so
# the unit leg is run with `-args -test.gocoverdir` to emit binary counters
# alongside the e2e ones. `go tool covdata merge` then folds both into one set
# and `textfmt` produces the single profile everything downstream reads.
#
# Why bother: the unit suites cover the packages a request passes through, and
# the e2e fleet covers the wiring between them — compose, TLS, the Entra
# handshake, the engines. Reporting only the first understates the project;
# reporting only the second understates the tests. One number over both is the
# only honest summary, and it is what the README badge publishes.
#
# Usage:
#   scripts/coverage_merge.sh [unit-covdata-dir] [e2e-covdata-dir]
#
# Both default to the conventional locations. A missing or empty e2e directory
# is NOT an error — it means no instrumented stack ran, and the script says so
# and reports the unit number alone, rather than failing a build for a
# measurement that was never attempted.
set -euo pipefail

cd "$(dirname "$0")/.."

UNIT_DIR=${1:-covdata/unit}
E2E_DIR=${2:-covdata}
OUT_DIR=covdata/merged
PROFILE=coverage-merged.out

have_counters() { [ -d "$1" ] && compgen -G "$1/covcounters.*" >/dev/null 2>&1; }

if ! have_counters "$UNIT_DIR"; then
  echo "no unit counters in $UNIT_DIR — run:" >&2
  echo "  mkdir -p $UNIT_DIR && go test -cover -coverpkg=./... ./... -args -test.gocoverdir=\$PWD/$UNIT_DIR" >&2
  exit 1
fi

inputs="$UNIT_DIR"
if have_counters "$E2E_DIR"; then
  inputs="$UNIT_DIR,$E2E_DIR"
  echo "merging unit + e2e counters"
else
  # Loud, not silent: a build that reports the unit number while believing it
  # reported both is how an instrumentation regression hides for weeks.
  echo "::warning::no e2e counters in $E2E_DIR — reporting UNIT coverage only." \
       "Did the stack run with docker-compose.coverage.yml, and exit via SIGTERM?"
fi

rm -rf "$OUT_DIR" && mkdir -p "$OUT_DIR"
go tool covdata merge -i="$inputs" -o="$OUT_DIR"
go tool covdata textfmt -i="$OUT_DIR" -o="$PROFILE"

total=$(go tool cover -func="$PROFILE" | tail -1 | awk '{print $3}' | tr -d '%')
echo "merged coverage: ${total}%  ($PROFILE)"
# Emitted for the caller (CI reads it to build the badge) rather than parsed
# back out of the human line above.
if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "total=${total}" >> "$GITHUB_OUTPUT"
fi
