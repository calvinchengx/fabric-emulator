# MicrosoftPowerBIMgmt — Microsoft's own PowerShell module for the Power BI
# REST surface — run against the emulator.
#
# Built from the official PowerShell tarball rather than
# mcr.microsoft.com/powershell, which publishes only linux/amd64 and 32-bit
# linux/arm. The tarball ships x64 AND arm64, so this builds natively on a
# developer Mac and on the amd64 CI runner alike. Same reasoning as
# e2e/deployment-pipelines, and the same PS_VERSION.
FROM mirror.gcr.io/library/debian:13-slim@sha256:89c1ea91e897f0fe0a3731595330bc45b8bdef4047464a07116f891a94054b31

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
# PSGallery intermittently 404s a package it lists (2026-10-27: one of six
# fetches of MicrosoftPowerBIMgmt.Reports/1.3.84 failed), and Install-Module
# reports that as a non-terminating error -- pwsh exited 0 and the build
# shipped an image with no module, which surfaced only at Import-Module in the
# driver. So: errors stop, the install retries, and the Import-Module here
# fails the build rather than the run.
RUN pwsh -NoProfile -Command \
      "\$ErrorActionPreference = 'Stop'; \
       Set-PSRepository PSGallery -InstallationPolicy Trusted; \
       foreach (\$attempt in 1..4) { \
         try { Install-Module MicrosoftPowerBIMgmt -RequiredVersion ${PBI_VERSION} -Scope AllUsers -Force -AllowClobber; break } \
         catch { \
           if (\$attempt -eq 4) { throw }; \
           Write-Warning \"Install-Module failed (attempt \$attempt/4): \$_\"; \
           Start-Sleep (\$attempt * 15) \
         } \
       }; \
       Import-Module MicrosoftPowerBIMgmt"

COPY driver.ps1 /driver.ps1
ENTRYPOINT ["pwsh", "-NoProfile", "-File", "/driver.ps1"]
