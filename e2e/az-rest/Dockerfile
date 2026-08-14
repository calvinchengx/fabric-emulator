# Microsoft's published Azure CLI image, unmodified. The driver is a
# thin harness around `az login` + `az rest`; nothing in the CLI is patched.
# Pin a versioned Azure Linux tag — `latest` would make a CLI release our
# flake, and Alpine tags stopped at 2.63.0.
FROM mcr.microsoft.com/azure-cli:2.89.0-azurelinux3.0
# Fail the build here if this tag stops shipping python3 — the driver is
# python, and a missing interpreter would otherwise surface as a cryptic
# entrypoint error after compose is already up.
RUN python3 --version
COPY driver.py /driver.py
ENTRYPOINT ["python3", "/driver.py"]
