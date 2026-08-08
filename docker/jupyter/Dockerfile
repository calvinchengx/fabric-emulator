# JupyterLab against the emulator family — a real editor, shipped not built.
#
# docs/44 records why the portal has no notebook editor: an editor we write is
# a client re-implementation with its own semantics, and users would then
# develop against a path no real Fabric client takes. The same doc records the
# one place that doctrine is weak — a newcomer running `make up` cannot touch a
# notebook without installing something first. This closes that the way
# OpenMetadata closes the catalog UI: attach the real thing.
FROM python:3.12-slim
COPY --from=ghcr.io/astral-sh/uv:0.11.32 /uv /uvx /bin/

WORKDIR /app
COPY pyproject.toml uv.lock ./
COPY python/ ./python/
RUN uv sync --frozen --no-install-project --group jupyter

ENV PATH="/app/.venv/bin:${PATH}"
# The shim is importable as `notebookutils`, exactly as it is in a Fabric
# notebook, so a notebook authored here runs unmodified there.
ENV PYTHONPATH="/app/python"

WORKDIR /notebooks
EXPOSE 8888
