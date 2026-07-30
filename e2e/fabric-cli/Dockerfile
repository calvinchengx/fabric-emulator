# The fab client: Microsoft's official Fabric CLI, driven against the emulator.
FROM python:3.12-slim
COPY --from=ghcr.io/astral-sh/uv:0.11.32 /uv /uvx /bin/
RUN apt-get update && apt-get install -y --no-install-recommends openssl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY pyproject.toml uv.lock ./
COPY python/ ./python/
RUN uv sync --frozen --no-install-project --group fabric-cli
ENV PATH="/app/.venv/bin:${PATH}"
COPY e2e/fabric-cli/driver.sh /driver.sh
ENTRYPOINT ["bash", "/driver.sh"]
