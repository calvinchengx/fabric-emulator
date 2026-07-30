# LakeSail's Sail: a Rust Spark-Connect server — PySpark without the JVM.
# There is no official Sail image; pysail ships prebuilt manylinux wheels.
# pyspark-client is installed alongside because the server imports
# pyspark.__version__ to select its client-compat config table.
FROM python:3.12-slim
COPY --from=ghcr.io/astral-sh/uv:0.11.32 /uv /uvx /bin/
WORKDIR /app
COPY pyproject.toml uv.lock ./
COPY python/ ./python/
RUN uv sync --frozen --no-install-project --group sail
COPY docker/sail/launcher.py /launcher.py
ENV PATH="/app/.venv/bin:${PATH}"
# Spark Connect (plain h2c gRPC — front with a proxy if TLS is needed).
EXPOSE 50051
ENTRYPOINT ["python", "/launcher.py"]
CMD ["spark", "server", "--ip", "0.0.0.0", "--port", "50051"]
