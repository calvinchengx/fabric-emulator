# Spark 3.5.3 (Hadoop 3.3.4) with the ABFS connector, delta-spark, and a
# small custom token provider that authenticates to entra-emulator.
FROM apache/spark:3.5.3

USER root

# javac for the token provider + the ABFS jars (also used at runtime, so we
# don't pay an Ivy resolve on every job).
ARG HADOOP_AZURE=3.3.4
RUN apt-get update && apt-get install -y --no-install-recommends openjdk-11-jdk-headless curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /opt/spark/jars
RUN curl -sSLO https://repo1.maven.org/maven2/org/apache/hadoop/hadoop-azure/${HADOOP_AZURE}/hadoop-azure-${HADOOP_AZURE}.jar \
    && curl -sSLO https://repo1.maven.org/maven2/com/microsoft/azure/azure-storage/7.0.1/azure-storage-7.0.1.jar \
    && curl -sSLO https://repo1.maven.org/maven2/org/wildfly/openssl/wildfly-openssl/1.0.7.Final/wildfly-openssl-1.0.7.Final.jar \
    && curl -sSLO https://repo1.maven.org/maven2/io/delta/delta-spark_2.12/3.2.0/delta-spark_2.12-3.2.0.jar \
    && curl -sSLO https://repo1.maven.org/maven2/io/delta/delta-storage/3.2.0/delta-storage-3.2.0.jar

# Compile + jar the token provider against the ABFS interface.
COPY EntraTokenProvider.java /tmp/src/com/calvinchengx/fabricemu/EntraTokenProvider.java
RUN javac -cp "/opt/spark/jars/*" -d /tmp/classes /tmp/src/com/calvinchengx/fabricemu/EntraTokenProvider.java \
    && jar cf /opt/spark/jars/fabric-emu-token-provider.jar -C /tmp/classes . \
    && rm -rf /tmp/src /tmp/classes

USER spark
