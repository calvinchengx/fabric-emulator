#!/usr/bin/env bash
# Make covdata/ writable by the uid the emulator container runs as.
#
# The image is distroless nonroot (uid 65532) and a bind mount keeps the HOST's
# ownership, so a directory created by the checkout user is not writable by the
# container. Go's coverage runtime does not shout about that: it writes nothing,
# leaving an empty directory that reads as "the e2e exercised nothing" rather
# than "the e2e could not say".
#
# Docker Desktop on macOS ignores the uid mismatch entirely, so this passes on a
# laptop and fails only on Linux CI — the same trap e2e/engine-matrix hit first,
# and for the same reason (see ensure_out_writable there).
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p covdata covdata/unit
chmod 777 covdata covdata/unit
echo "covdata/ prepared: $(stat -c '%a' covdata 2>/dev/null || stat -f '%Lp' covdata)"
