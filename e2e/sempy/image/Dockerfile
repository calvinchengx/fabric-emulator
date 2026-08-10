# sempy needs a .NET runtime for its XMLA path (pythonnet + the ADOMD.NET and
# Tabular assemblies it ships), so the SDK image is the base rather than a
# python one. linux/amd64 is not optional: the NuGet payload is amd64-only.
FROM mcr.microsoft.com/dotnet/sdk:8.0
RUN apt-get -qq update \
 && apt-get -qq install -y --no-install-recommends python3 python3-venv \
 && rm -rf /var/lib/apt/lists/*
RUN python3 -m venv /v && /v/bin/pip -q install --no-input semantic-link-sempy
ENV PYTHONNET_RUNTIME=coreclr
