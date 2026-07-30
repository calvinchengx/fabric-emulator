FROM python:3.12-slim
COPY --from=ghcr.io/astral-sh/uv:0.11.32 /uv /uvx /bin/

ARG UV_GROUP
WORKDIR /app

COPY pyproject.toml uv.lock ./
COPY python/ ./python/
RUN uv sync --frozen --no-install-project --group "${UV_GROUP}"

ENV PATH="/app/.venv/bin:${PATH}"
