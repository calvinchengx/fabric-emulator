# Apache Spark 3.5.5 + Delta 3.2 + Java 11 + Scala 2.12 matches Microsoft
# Fabric Runtime 1.3's public engine baseline. The short `apache/spark:3.5.5`
# tag is Java 17; Fabric is Java 11, so the java11 variant is the pin, not the
# floating short tag. This image is an optional compatibility oracle; Sail
# stays the lightweight default engine used by docker compose.

# ---------------------------------------------------------------- build stage
#
# Everything that needs a COMPILER happens here, so the final image never
# carries one.
#
# This used to `apt-get install openjdk-11-jdk-headless` into the Spark image,
# and that was never sound: the base ships a Temurin JRE as a TARBALL under
# /opt/java/openjdk (11.0.24), not as an apt package, so Ubuntu's JDK insists on
# its own `openjdk-11-jre-headless` at a different version. apt cannot reconcile
# two JREs and answers `E: Unable to correct problems, you have held broken
# packages`. It worked only while the versions happened to line up; when the
# 20.04 index moved to 11.0.27 the engine-matrix job went red and STAYED red —
# a hard break from someone else's release, not a flake to retry.
#
# Copying a JDK from the Temurin image instead has no package manager in it to
# disagree with anything, and the compiler never reaches the runtime image.
FROM apache/spark:3.5.5-scala2.12-java11-python3-ubuntu AS build
USER root
COPY --from=eclipse-temurin:11-jdk /opt/java/openjdk /opt/jdk

ARG CURL_RETRY="--retry 5 --retry-connrefused --retry-max-time 180"
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
# curl below is the FALLBACK rather than the normal path. (curl is already in
# the base image — the apt line that used to install it was redundant.)
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

# Compiled against the jars acquired above, which is why this stage acquires
# them rather than the final one: the token provider implements a hadoop-azure
# interface that is not in the base image.
COPY docker/spark-runtime/EntraTokenProvider.java /tmp/src/com/calvinchengx/fabricemu/EntraTokenProvider.java
RUN /opt/jdk/bin/javac -cp "/opt/spark/jars/*" -d /tmp/classes \
        /tmp/src/com/calvinchengx/fabricemu/EntraTokenProvider.java \
    && /opt/jdk/bin/jar cf /opt/spark/jars/fabric-emu-token-provider.jar -C /tmp/classes . \
    && rm -rf /tmp/src /tmp/classes

# --------------------------------------------------------------- runtime stage
FROM apache/spark:3.5.5-scala2.12-java11-python3-ubuntu
COPY --from=ghcr.io/astral-sh/uv:0.11.32 /uv /uvx /bin/
USER root
# The jars, including the compiled token provider. No compiler, no apt, no
# network — the runtime image is the base plus a directory.
COPY --from=build /opt/spark/jars /opt/spark/jars
# Livy agent modules (eventstream_kafka.py and siblings). Bind-mounted in
# docker-compose.spark-jvm.yml for local iteration; baked here for the published tag.
COPY python/spark_agent/*.py /opt/spark/work-dir/
USER spark

# The python a notebook actually needs. The runtime is Spark plus jars, which is
# enough to START the agent but not to run anything real: agent.py imports only
# stdlib, so the gap only appears once a notebook imports a framework. `requests`
# is the one nearly every such framework pulls in, and without it a notebook on
# this overlay dies on `ModuleNotFoundError: No module named 'requests'` after
# the agent has reported ready, so it reads as a notebook fault rather than a
# missing runtime.
#
# Kept to the same pin the python runtime resolves, and installed as root before
# dropping back to spark so it lands in the image rather than a user directory a
# restart discards.
USER root
RUN pip install --no-cache-dir requests==2.32.4
USER spark
