# The fab client: Microsoft's official Fabric CLI, driven against the emulator.
FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends openssl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir ms-fabric-cli
COPY driver.sh /driver.sh
ENTRYPOINT ["bash", "/driver.sh"]
