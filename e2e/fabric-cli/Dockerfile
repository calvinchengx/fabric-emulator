# The fab client: Microsoft's official Fabric CLI, driven against the emulator.
FROM python:3.12-slim
COPY --from=ghcr.io/astral-sh/uv:0.11.32 /uv /uvx /bin/
RUN apt-get update && apt-get install -y --no-install-recommends openssl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# THIS DIRECTORY'S OWN project, not the root workspace's. `ms-fabric-cli` pins
# `pyyaml==6.0.2` exactly, and while that pin sat in a root dependency group it
# took part in the root's universal resolution and held pyyaml down for the
# whole repository. See e2e/fabric-cli/pyproject.toml for the measurement.
#
# The root `python/` copy is gone with it: the sync was always
# `--no-install-project`, so the workspace members were never installed here,
# only resolved against.
COPY e2e/fabric-cli/pyproject.toml e2e/fabric-cli/uv.lock ./
RUN uv sync --frozen
ENV PATH="/app/.venv/bin:${PATH}"
COPY e2e/fabric-cli/driver.sh /driver.sh
ENTRYPOINT ["bash", "/driver.sh"]
