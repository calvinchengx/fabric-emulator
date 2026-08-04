# Apache Spark 3.5 + Delta 3.2 matches Microsoft Fabric Runtime 1.3's public
# engine baseline. This image is an optional compatibility oracle; Sail stays
# the lightweight default engine used by docker compose.
FROM apache/spark:3.5.3
COPY --from=ghcr.io/astral-sh/uv:0.11.32 /uv /uvx /bin/

USER root

# One retry policy for every fetch in this image, named once.
ARG CURL_RETRY="--retry 5 --retry-connrefused --retry-max-time 180"
RUN apt-get update && apt-get install -y --no-install-recommends openjdk-11-jdk-headless curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /opt/spark/jars

# The jar list and whatever has been pre-fetched into the build context. The
# directory holds only .gitkeep in a clean checkout, so a plain
# `docker compose build` still works exactly as it always did — it just fetches.
COPY docker/spark-runtime/jars.txt /tmp/jars.txt
COPY docker/spark-runtime/jars/ /tmp/jarcache/

# Use the pre-fetched jar when there is one, fetch it when there is not.
#
# Maven Central rate-limits GitHub-hosted runners (shared IP pools), and a 429
# here failed the whole engine-matrix job twice in one day — a CI red that reads
# as a regression and is really someone else's traffic. Retrying does not fix a
# sustained per-IP limit; not making the request does. CI pre-populates
# /tmp/jarcache via scripts/fetch_spark_jars.sh behind actions/cache, so the
# curl below is the FALLBACK rather than the normal path.
RUN set -eu; \
    while read -r url; do \
        case "$url" in ''|\#*) continue ;; esac; \
        name="$(basename "$url")"; \
        if [ -s "/tmp/jarcache/$name" ]; then \
            cp "/tmp/jarcache/$name" "/opt/spark/jars/$name"; \
        else \
            curl -fsSLO ${CURL_RETRY} "$url"; \
        fi; \
    done < /tmp/jars.txt; \
    rm -rf /tmp/jarcache /tmp/jars.txt

COPY docker/spark-runtime/EntraTokenProvider.java /tmp/src/com/calvinchengx/fabricemu/EntraTokenProvider.java
RUN javac -cp "/opt/spark/jars/*" -d /tmp/classes /tmp/src/com/calvinchengx/fabricemu/EntraTokenProvider.java \
    && jar cf /opt/spark/jars/fabric-emu-token-provider.jar -C /tmp/classes . \
    && rm -rf /tmp/src /tmp/classes

USER spark
