# Microsoft's published Azure CLI image, unmodified — the same pin e2e/az-rest
# uses, for the same reason: `latest` would make a CLI release our flake.
#
# The build context is e2e/ rather than this directory so az-rest's driver can
# be copied in as a MODULE. Its ~200 lines of login (cert harvesting, the ARM
# stub `az login` insists on, private-cloud registration) are hard-won and
# identical here; duplicating them would mean two copies drifting apart, and
# the copy that drifts is the one nobody is looking at.
FROM mcr.microsoft.com/azure-cli:2.89.0-azurelinux3.0
RUN python3 --version
COPY az-rest/driver.py /azrest.py
COPY pipeline-activities/driver.py /driver.py
ENTRYPOINT ["python3", "/driver.py"]
