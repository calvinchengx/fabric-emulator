# Microsoft's Fabric CLI, and nothing else.
#
# Deliberately NOT built from the repo (e2e/fabric-cli's image copies this
# project's pyproject.toml and uv.lock). This example is meant to be copied out
# and adapted, so it must build from its own directory alone.
#
# The version is pinned because `fab`'s verb set and output shape move between
# releases — see the version note at the end of ../../docs/34-fab-driven-example.md.
# It lives in requirements.txt, with every dependency's hash beside it; the base
# image is pinned by digest for the same reason, so a rebuild is the same image.
FROM mirror.gcr.io/library/python:3.12-slim@sha256:aa943ce411423c7582288f18707ff66eddcbaeb510502d747a72aeb34f30d159

COPY requirements.txt /tmp/requirements.txt

# openssl is not incidental: it is how the entrypoint learns the emulator's
# self-signed certificates, which is the whole reason `fab` can speak TLS to
# hostnames no public CA has ever signed.
RUN apt-get update \
    && apt-get install -y --no-install-recommends openssl ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir --require-hashes -r /tmp/requirements.txt \
    && rm /tmp/requirements.txt

COPY entrypoint.sh /entrypoint.sh
ENTRYPOINT ["bash", "/entrypoint.sh"]
