# LakeSail's Sail: a Rust Spark-Connect server — PySpark without the JVM.
# There is no official Sail image; this follows upstream's quickstart pattern
# (pip install pysail, prebuilt manylinux wheels for x86_64/aarch64).
# pyspark-client is installed alongside because the server imports
# pyspark.__version__ to select its client-compat config table.
FROM python:3.12-slim
ARG PYSAIL_VERSION=0.6.6
ARG PYSPARK_CLIENT_VERSION=4.2.0
RUN pip install --no-cache-dir "pysail==${PYSAIL_VERSION}" "pyspark-client==${PYSPARK_CLIENT_VERSION}"
COPY launcher.py /launcher.py
# Spark Connect (plain h2c gRPC — front with a proxy if TLS is needed).
EXPOSE 50051
ENTRYPOINT ["python", "/launcher.py"]
CMD ["spark", "server", "--ip", "0.0.0.0", "--port", "50051"]
