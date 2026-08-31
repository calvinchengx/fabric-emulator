# The Spark statement-executor agent — the engine side of Livy and RunNotebook.
#
# SELF-CONTAINED, and that is the point. This image was built from
# docker/python-runtime/Dockerfile, which installs dependencies and nothing
# else; the agent itself arrived through a `./e2e/livy:/livy` bind mount that
# every compose file in this repository happened to set. Inside the project
# that is invisible. From outside it is fatal: a consumer pulling the published
# tag got a container with no agent in it, so RunNotebook jobs never reached a
# terminal state — since 0.14.0, a NotebookError on timeout.
#
# The preamble below is shared with docker/python-runtime/Dockerfile on
# purpose, and test_the_runtime_images_share_one_preamble fails if the two
# drift. Duplicated deliberately rather than factored out: a base image would
# have to be built and pushed before this one, which is a release-ordering
# constraint for six lines.
FROM python:3.14-slim
COPY --from=ghcr.io/astral-sh/uv:0.11.32 /uv /uvx /bin/

WORKDIR /app

# kafka-python GSSAPI has no manylinux wheel; compile against MIT krb5.
RUN apt-get update && apt-get install -y --no-install-recommends \
      gcc libkrb5-dev libkrb5-3 libgssapi-krb5-2 \
    && rm -rf /var/lib/apt/lists/*

COPY pyproject.toml uv.lock ./
COPY python/ ./python/
# spark-agent-dbt, which includes sail-delta: the agent routes
# OPTIMIZE/VACUUM/CDF through delta-rs, so `deltalake` belongs in the image
# (with spark-connect the import failed and the interception silently never
# installed), and it also runs dbt for databricks-emulator's `dbt_task`, which
# hands this image a project and a profile rather than a statement.
# --no-cache: uv's DOWNLOAD cache is build input, and it was being baked into
# the runtime image -- 538 MB of it in the published 4.2.0, for wheels nothing
# unpacks again. Separable from the dbt addition and fixed here because it is
# the same line: without it this image would appear to double when the actual
# cost of dbt is the venv, +219 MB.
RUN uv sync --frozen --no-install-project --no-cache --group spark-agent-dbt

ENV PATH="/app/.venv/bin:${PATH}"
# Same reason as docker/python-runtime, and set here too because the two images
# are required to share one preamble (python/tests/test_spark_agent_packaging.py):
# this agent also writes its progress to a pipe, where python block-buffers
# stdout, so a run killed mid-flight prints nothing and its silence locates
# nothing.
ENV PYTHONUNBUFFERED=1

# Runnable with no command and no mount — the whole repair. Everything the
# agent still needs is environment (SPARK_REMOTE, the Entra and storage
# variables); nothing it needs is a file from the host.
# THE RUNTIME THIS IMAGE TARGETS, declared rather than inferred.
#
# Framework-conformance contract 3 (docs/38 §3): a Fabric Runtime pins Spark,
# Delta, Python and a library set together as one versioned unit, and a
# framework declares which runtime it targets and assumes that floor. Two images
# with different Python versions and no statement about which runtime either
# claimed is how the JVM overlay shipped Python 3.8 under a "Runtime 1.3"
# heading for months.
#
# This declares the FLOOR the image targets -- interpreter and library
# versions -- not that its engine is Spark. Whether the engine behaves like
# Spark is the engine matrix's question, and it answers it row by row.
ENV FABRIC_RUNTIME=1.3
CMD ["python3", "/app/python/spark_agent/agent.py"]
