"""TLS verification scoped to the hosts whose certificate is actually untrusted.

WHY THIS EXISTS, and it is the same finding one language over. `internal/akv`
turned certificate verification off for its whole TRANSPORT from a flag
documented for entra-emulator's self-signed certificate, on the one client
whose allowlist deliberately admits a real `*.vault.azure.net` — so a secret
could come back from a real vault over a connection nobody authenticated, while
the https requirement that exists for exactly that reason still passed. The fix
there was `akv.hostScopedTLS`: decide per request HOST rather than per client,
so the skip reaches the one configured emulator and a real Azure domain always
verifies. See internal/akv/akv.go and docs/63-security-footguns.md §4.

This agent's outbound mints had the same shape in the same way. Each builds an
`ssl` context with `check_hostname = False` and `verify_mode = ssl.CERT_NONE`
unconditionally, under a comment saying the issuer is entra-emulator on the
compose network — and each then POSTs `ENTRA_CLIENT_SECRET`, or carries a
bearer token, to a URL that is CONFIGURATION (`ENTRA_TOKEN_URL`,
`ENTRA_FORGE_URL`, `FABRIC_API_URL`). The comment described the compose case
correctly and the code did not restrict itself to it, so pointing any of those
at a real endpoint sent the credential over an unauthenticated connection with
nothing in the path saying so. `FABRIC_API_URL` makes it concrete: it DEFAULTS
to `api.fabric.microsoft.com`.

So the skip is scoped to hosts that are local or emulator-shaped, and every
other host — including every real Microsoft domain — gets full default
verification. `None` is returned rather than a permissive context in that case,
because `None` is what `urllib.request.urlopen` reads as "use the default", and
a helper that returned a hand-built strict context would be a second place for
the default to drift from the interpreter's.

An operator who must point one of these at a dotted non-local host serving a
self-signed certificate installs the local CA, which
docs/05-tls-and-hosts.md already documents as the supported route.
"""
import ssl
import urllib.parse

# Hosts that are this machine, or a sibling container in the compose network.
# Exact matches; the shape rules below cover the rest.
_LOCAL_EXACT = frozenset({
    "localhost", "127.0.0.1", "::1", "[::1]", "0.0.0.0",
    # Docker Desktop's name for the host from inside a container. Dotted, so it
    # would otherwise fall to the "has a dot, verify it" rule below.
    "host.docker.internal",
})


def is_local_host(host):
    """Is this hostname local, or a compose service name?

    Four shapes, and the BARE-NAME one is the load-bearing case here: compose
    resolves service names (`entra-emulator`, `fabric-emulator`) with no dot in
    them, which is how every URL in this repository's own compose files is
    spelled. A name with no dot cannot be a public DNS name, so treating it as
    local is not a guess.

    `.localhost` is reserved to the loopback interface by RFC 6761, and
    `.internal` is reserved for private use by RFC 6762 / the ICANN board's
    2024 reservation — neither can resolve to a public endpoint.
    """
    host = (host or "").strip().lower()
    if not host:
        return False
    if host in _LOCAL_EXACT:
        return True
    if host.endswith(".localhost") or host.endswith(".internal"):
        return True
    # The whole 127.0.0.0/8 loopback block, not just 127.0.0.1.
    if host.startswith("127."):
        return True
    # No dot at all: a compose service name or a bare machine name.
    return "." not in host


def tls_context(url):
    """An unverified context for a local host, else None (default verification).

    `None` means "whatever the interpreter's default is", which for
    `urllib.request.urlopen` is a fully verifying context. That is the point:
    a real endpoint is verified, and only the emulator's self-signed
    certificate is accepted.
    """
    host = urllib.parse.urlsplit(url or "").hostname
    if not is_local_host(host):
        return None
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    return ctx
