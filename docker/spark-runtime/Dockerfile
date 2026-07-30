# Apache Spark 3.5 + Delta 3.2 matches Microsoft Fabric Runtime 1.3's public
# engine baseline. This image is an optional compatibility oracle; Sail stays
# the lightweight default engine used by docker compose.
FROM apache/spark:3.5.3

USER root

ARG HADOOP_AZURE_VERSION=3.3.4
ARG DELTA_VERSION=3.2.0
RUN apt-get update && apt-get install -y --no-install-recommends openjdk-11-jdk-headless curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /opt/spark/jars
RUN curl -fsSLO "https://repo1.maven.org/maven2/org/apache/hadoop/hadoop-azure/${HADOOP_AZURE_VERSION}/hadoop-azure-${HADOOP_AZURE_VERSION}.jar" \
    && curl -fsSLO https://repo1.maven.org/maven2/com/microsoft/azure/azure-storage/7.0.1/azure-storage-7.0.1.jar \
    && curl -fsSLO https://repo1.maven.org/maven2/org/wildfly/openssl/wildfly-openssl/1.0.7.Final/wildfly-openssl-1.0.7.Final.jar \
    && curl -fsSLO "https://repo1.maven.org/maven2/io/delta/delta-spark_2.12/${DELTA_VERSION}/delta-spark_2.12-${DELTA_VERSION}.jar" \
    && curl -fsSLO "https://repo1.maven.org/maven2/io/delta/delta-storage/${DELTA_VERSION}/delta-storage-${DELTA_VERSION}.jar"

COPY docker/spark-runtime/EntraTokenProvider.java /tmp/src/com/calvinchengx/fabricemu/EntraTokenProvider.java
RUN javac -cp "/opt/spark/jars/*" -d /tmp/classes /tmp/src/com/calvinchengx/fabricemu/EntraTokenProvider.java \
    && jar cf /opt/spark/jars/fabric-emu-token-provider.jar -C /tmp/classes . \
    && rm -rf /tmp/src /tmp/classes

USER spark
