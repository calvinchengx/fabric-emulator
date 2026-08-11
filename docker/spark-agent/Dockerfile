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
FROM python:3.12-slim
COPY --from=ghcr.io/astral-sh/uv:0.11.32 /uv /uvx /bin/

WORKDIR /app

COPY pyproject.toml uv.lock ./
COPY python/ ./python/
# sail-delta, not spark-connect: the agent routes OPTIMIZE/VACUUM/CDF through
# delta-rs, so `deltalake` belongs in the image. With spark-connect the import
# failed and the interception silently never installed.
RUN uv sync --frozen --no-install-project --group sail-delta

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
CMD ["python3", "/app/python/spark_agent/agent.py"]
