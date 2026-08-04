#!/usr/bin/env bash
# Pre-populate the Spark jars into the build context so the image build needs no
# network for them.
#
# Maven Central rate-limits GitHub-hosted runners — they share IP pools — and a
# 429 here failed the whole engine-matrix job twice in one day, reading as a
# regression when it was someone else's traffic. curl already retries; retrying
# harder does not help when the limit is per-IP and sustained. Caching the files
# removes the request instead.
#
# Idempotent: a jar already present is left alone, so a cache hit costs nothing.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
list="$here/docker/spark-runtime/jars.txt"
dest="$here/docker/spark-runtime/jars"
mkdir -p "$dest"

fetched=0 cached=0
while read -r url; do
    case "$url" in ''|\#*) continue ;; esac
    name="$(basename "$url")"
    if [ -s "$dest/$name" ]; then
        cached=$((cached + 1))
        continue
    fi
    echo "fetching $name"
    curl -fsSL --retry 5 --retry-connrefused --retry-max-time 180 -o "$dest/$name.part" "$url"
    mv "$dest/$name.part" "$dest/$name"
    fetched=$((fetched + 1))
done < "$list"
echo "spark jars: $fetched fetched, $cached already cached"
