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

A DELIBERATE MIRROR, named here so it cannot be mistaken for an accident:
`fabric_target._is_local_host` and `fabric_target._tls_context` are
near-identical copies of the two functions below. `fabric-target` ships as its
own distributable and cannot import `spark_agent`, so the duplication is the
price of that packaging boundary — the same arrangement as `akv.hostScopedTLS`
and this file, one language apart, and stated in both copies for the same
reason. A correction to either must be made to BOTH: that is how the two
fail-opens `is_local_host` describes were repaired, in two places, each pinned
by a host table in its own package's suite
(python/tests/test_spark_agent_tls_scope.py and
python/fabric-target/tests/test_fabric_target.py).
"""
import ipaddress
import ssl
import urllib.parse

# NAMES that are this machine, or a sibling container in the compose network.
# Address LITERALS are deliberately absent: `is_local_host` decides those on
# the PARSED address before this set is consulted. `127.0.0.1`, `::1`, `[::1]`
# and `0.0.0.0` were all entries here once and can no longer match anything,
# which is worse than absent — a dead entry reads as load-bearing, so the next
# reader would believe this set is what admits loopback.
_LOCAL_EXACT = frozenset({
    "localhost",
    # Docker Desktop's name for the host from inside a container. Dotted, so it
    # would otherwise fall to the "has a dot, verify it" rule below.
    "host.docker.internal",
})


def is_local_host(host):
    """Is this hostname local, or a compose service name?

    AN ADDRESS LITERAL IS DECIDED ON THE PARSED ADDRESS, before any name rule
    runs — and that branch is a repair rather than a refinement, because the
    name rules alone failed OPEN twice, at every credential-carrying caller in
    this package:

      * `"." not in host` read EVERY IPv6 literal as local, since no IPv6
        address contains a dot. `https://[2606:4700:4700::1111]/token` got an
        unverified context, and so did the Microsoft-owned `2620:1ec:bdf::1`.
      * `host.startswith("127.")` is a prefix test against a NAME, so the
        attacker-chosen public name `127.0.0.1.evil.com` got one too.

    Both were reachable from configuration (`ENTRA_TOKEN_URL`,
    `FABRIC_API_URL`, `ENTRA_FORGE_URL`), which is the same "the URL is
    configuration" argument this module exists to make — and the dotless
    reasoning below was never meant to cover an address literal. Loopback,
    link-local and private addresses are local, which keeps the docker bridge
    (`172.17.0.2`), IPv6 ULA (`fc00::1`) and a scoped link-local
    (`fe80::1%eth0`) on the compose path; every global-unicast address
    verifies.

    Then the NAME rules, of which the BARE-NAME one is load-bearing here:
    compose resolves service names (`entra-emulator`, `fabric-emulator`) with
    no dot in them, which is how every URL in this repository's own compose
    files is spelled. A name with no dot cannot be a public DNS name, so
    treating it as local is not a guess.

    `.localhost` is reserved to the loopback interface by RFC 6761, and
    `.internal` is reserved for private use by RFC 6762 / the ICANN board's
    2024 reservation — neither can resolve to a public endpoint.
    """
    host = (host or "").strip().lower()
    if not host:
        return False
    try:
        addr = ipaddress.ip_address(host)
    except ValueError:
        pass
    else:
        return addr.is_loopback or addr.is_link_local or addr.is_private
    if host in _LOCAL_EXACT:
        return True
    if host.endswith(".localhost") or host.endswith(".internal"):
        return True
    # A colon-bearing host that `ip_address` could not parse is a MALFORMED
    # address literal rather than a DNS name, and the dotless rule below would
    # read it as local. Fail closed: verify it.
    if ":" in host:
        return False
    # No dot at all: a compose service name or a bare machine name.
    return "." not in host


def tls_context(url):
    """An unverified context for a local host, else None (default verification).

    `None` means "whatever the interpreter's default is", which for
    `urllib.request.urlopen` is a fully verifying context. That is the point:
    a real endpoint is verified, and only the emulator's self-signed
    certificate is accepted.

    A URL WHOSE HOST CANNOT BE PARSED IS VERIFIED, deliberately rather than
    incidentally. `urlsplit` defers host validation to `.hostname`, which
    raises `ValueError` on a malformed bracketed literal
    (`https://[not:an:address]/`); uncaught, that exception escapes into the
    middle of a token mint — fail-closed in that nothing is transmitted, but a
    path nobody chose and a test could only "tolerate". Caught here, so a
    malformed URL takes the same branch as a public one and the caller gets a
    verifying connection or a clean transport error.
    """
    try:
        host = urllib.parse.urlsplit(url or "").hostname
    except ValueError:
        return None
    if not is_local_host(host):
        return None
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    return ctx
