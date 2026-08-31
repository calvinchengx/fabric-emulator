FROM python:3.14-slim
COPY --from=ghcr.io/astral-sh/uv:0.11.32 /uv /uvx /bin/

ARG UV_GROUP
WORKDIR /app

# kafka-python GSSAPI has no manylinux wheel; compile against MIT krb5.
RUN apt-get update && apt-get install -y --no-install-recommends \
      gcc libkrb5-dev libkrb5-3 libgssapi-krb5-2 \
    && rm -rf /var/lib/apt/lists/*

COPY pyproject.toml uv.lock ./
COPY python/ ./python/
RUN uv sync --frozen --no-install-project --group "${UV_GROUP}"

ENV PATH="/app/.venv/bin:${PATH}"
# Every driver on this image writes its progress to a pipe, where python
# block-buffers stdout: the e2e/sail driver's twenty prints all carried ONE
# timestamp, flushed at exit, so a run killed mid-flight printed nothing at all
# and its 24 minutes of silence said nothing about where it stopped. Progress is
# only worth printing if a hang can be located by it. Last, so the uv layer
# above stays cached for all 18 stacks built from this file.
ENV PYTHONUNBUFFERED=1
