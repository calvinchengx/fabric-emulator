# MicrosoftPowerBIMgmt — Microsoft's own PowerShell module for the Power BI
# REST surface — run against the emulator.
#
# Built from the official PowerShell tarball rather than
# mcr.microsoft.com/powershell, which publishes only linux/amd64 and 32-bit
# linux/arm. The tarball ships x64 AND arm64, so this builds natively on a
# developer Mac and on the amd64 CI runner alike. Same reasoning as
# e2e/deployment-pipelines, and the same PS_VERSION.
FROM debian:12-slim

ARG TARGETARCH
ARG PS_VERSION=7.4.18

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl openssl libicu72 \
    && rm -rf /var/lib/apt/lists/* \
    && case "$TARGETARCH" in \
         amd64) PS_ARCH=x64 ;; \
         arm64) PS_ARCH=arm64 ;; \
         *) echo "unsupported TARGETARCH=$TARGETARCH" >&2; exit 1 ;; \
       esac \
    && curl -fsSL --retry 5 --retry-connrefused --retry-max-time 180 -o /tmp/pwsh.tar.gz \
       "https://github.com/PowerShell/PowerShell/releases/download/v${PS_VERSION}/powershell-${PS_VERSION}-linux-${PS_ARCH}.tar.gz" \
    && mkdir -p /opt/microsoft/powershell/7 \
    && tar zxf /tmp/pwsh.tar.gz -C /opt/microsoft/powershell/7 \
    && chmod +x /opt/microsoft/powershell/7/pwsh \
    && ln -s /opt/microsoft/powershell/7/pwsh /usr/bin/pwsh \
    && rm /tmp/pwsh.tar.gz

# Pinned: an unpinned module moves on its own, and a witness that changes
# underneath the claim it supports is not evidence of anything.
ARG PBI_VERSION=1.3.84
RUN pwsh -NoProfile -Command \
      "Set-PSRepository PSGallery -InstallationPolicy Trusted; \
       Install-Module MicrosoftPowerBIMgmt -RequiredVersion ${PBI_VERSION} \
         -Scope AllUsers -Force -AllowClobber"

COPY driver.ps1 /driver.ps1
ENTRYPOINT ["pwsh", "-NoProfile", "-File", "/driver.ps1"]
