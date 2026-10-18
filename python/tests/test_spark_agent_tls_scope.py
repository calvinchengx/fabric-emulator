"""The host-scoped TLS boundary, asserted as a table — both copies of it.

WHY THIS FILE EXISTS, and it is this repository's own central thesis applied to
itself. `tls_scope` and `fabric_target`'s `_tls_context` were written to repair
six unconditional verification skips, five of which carry `ENTRA_CLIENT_SECRET`
or a bearer token to a URL that is CONFIGURATION — and when they landed,
NOTHING asserted either of them. `scripts/check_script_test_coverage.py` scopes
itself to `scripts/`, so no gate noticed: an 89-line module whose entire job is
deciding whether to authenticate a peer holding a client secret, with its
behaviour pinned by nothing. A guard whose behaviour nothing asserts can stop
guarding and go on reporting green (python/tests/test_make_check_runs_in_ci.py,
one level up).

It was not hypothetical for even one release. Both modules' docstrings made a
universally-quantified claim — every real Microsoft domain gets full default
verification — and both were FALSE in the permissive direction, twice over:

  * `return "." not in host` read every IPv6 LITERAL as local, because no IPv6
    address contains a dot. `https://[2606:4700:4700::1111]/token` got an
    unverified context, and so did the Microsoft-owned `2620:1ec:bdf::1`.
  * `host.startswith("127.")` is a prefix test against a NAME, so the
    attacker-chosen public name `127.0.0.1.evil.com` got one too.

A host/expectation table would have caught both on its first run, which is why
this is a table rather than a handful of cases. It covers BOTH copies from one
list: the two helpers are a deliberate mirror (fabric-target is a separate
distributable and cannot import the agent), and a mirror checked in one place
only is a mirror that drifts. `fabric_target` is imported by path rather than
through its dependency group because its module body is stdlib-only, so this
suite can hold both halves without the `fabric-target` extras installed.
"""
import pathlib
import ssl
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "spark_agent"))
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "fabric-target"))

import fabric_target  # noqa: E402
import tls_scope  # noqa: E402

# The two copies, named so a failure says WHICH boundary leaked.
IMPLS = [
    pytest.param(tls_scope.is_local_host, tls_scope.tls_context, id="spark_agent"),
    pytest.param(fabric_target._is_local_host, fabric_target._tls_context,
                 id="fabric_target"),
]

# Hosts the skip is FOR: this machine, or a sibling on a compose network.
LOCAL = [
    # Loopback, by name and by address, in both families. `::1` is the case the
    # dotless rule used to admit for the wrong reason.
    "localhost", "LOCALHOST", "127.0.0.1", "127.0.0.53", "::1", "0.0.0.0",
    # Bare compose service names — the load-bearing rule, since this is how
    # every URL in this repository's own compose files is spelled.
    "entra-emulator", "fabric-emulator", "sail", "spark-agent",
    # Reserved suffixes: RFC 6761 loopback, and private-use `.internal`.
    "box.localhost", "entra.localhost", "svc.internal", "db.internal",
    # Docker Desktop's name for the host, which is dotted and so needs the
    # exact-name set rather than a shape rule.
    "host.docker.internal",
    # Private and link-local addresses: the docker bridge, an IPv6 ULA, and a
    # scoped link-local. A compose network is exactly this, so these must stay
    # on the unverified path or the repair breaks the case it exists to serve.
    "172.17.0.2", "10.1.2.3", "192.168.1.9", "169.254.1.1", "fc00::1",
    "fe80::1%eth0",
]

# Hosts that MUST be verified. Every entry here was reachable from
# configuration at a call site carrying a credential.
MUST_VERIFY = [
    # The default `FABRIC_API_URL`, and the token endpoint beside it.
    "api.fabric.microsoft.com", "login.microsoftonline.com",
    "onelake.dfs.fabric.microsoft.com", "contoso.vault.azure.net",
    # GLOBAL-UNICAST IPv6 LITERALS — the fail-open this table was written for.
    # Dotless, so the name rules read all three as local; the last is
    # Microsoft-owned.
    # `2001:db8::/32` is deliberately NOT here: Python's `ipaddress` reports
    # the RFC 3849 documentation prefix as private, so it lands on the local
    # side — correct, since it is unroutable, and worth saying so rather than
    # leaving a reader to wonder why the obvious example address is absent.
    "2606:4700:4700::1111", "2001:4860:4860::8888", "2620:1ec:bdf::1",
    # A public NAME that merely starts with a loopback-shaped prefix, which the
    # old `startswith("127.")` admitted. An attacker chooses this one.
    "127.0.0.1.evil.com", "localhost.evil.com", "127.0.0.1.nip.io",
    # A dotted public address, and a malformed address literal: colon-bearing
    # and unparseable, so the dotless rule would have read it as local.
    "8.8.8.8", "not:an:address",
    # Nothing at all is not a local host. A URL with no host reaches here as
    # None or "", and guessing "local" would be the permissive guess.
    "", None,
]


def _unverified(ctx):
    """Is this context one that authenticates no peer?"""
    return ctx is not None and ctx.verify_mode == ssl.CERT_NONE \
        and ctx.check_hostname is False


@pytest.mark.parametrize("is_local,_ctx", IMPLS)
@pytest.mark.parametrize("host", LOCAL)
def test_a_local_or_compose_host_is_recognised(host, is_local, _ctx):
    assert is_local(host) is True, host


@pytest.mark.parametrize("is_local,_ctx", IMPLS)
@pytest.mark.parametrize("host", MUST_VERIFY)
def test_every_other_host_is_not_local(host, is_local, _ctx):
    assert is_local(host) is False, host


@pytest.mark.parametrize("_is_local,tls_ctx", IMPLS)
@pytest.mark.parametrize("host", LOCAL)
def test_a_local_host_gets_the_unverified_context(host, _is_local, tls_ctx):
    """The decision, through the function the callers actually use.

    Asserted on the CONTEXT rather than on `is_local_host` alone, because a
    `tls_context` that stopped consulting the host rule would satisfy every
    predicate test above and still hand an unverified context to everything.
    """
    bracketed = f"[{host}]" if ":" in host else host
    assert _unverified(tls_ctx(f"https://{bracketed}:8443/token")), host


@pytest.mark.parametrize("_is_local,tls_ctx", IMPLS)
@pytest.mark.parametrize("host", [h for h in MUST_VERIFY if h])
def test_every_other_host_gets_full_default_verification(host, _is_local, tls_ctx):
    """`None`, which `urllib.request.urlopen` reads as the default.

    The claim both docstrings make, pinned: a real Microsoft endpoint — named,
    or spelled as an IPv6 literal — is authenticated. This is the assertion
    that fails on the pre-repair code, for `2606:4700:4700::1111` and for
    `127.0.0.1.evil.com`.
    """
    bracketed = f"[{host}]" if ":" in host else host
    assert tls_ctx(f"https://{bracketed}/token") is None, host


@pytest.mark.parametrize("_is_local,tls_ctx", IMPLS)
def test_an_unparseable_url_is_verified_rather_than_raising(_is_local, tls_ctx):
    """A malformed bracketed host is a VERIFIED connection, not a traceback.

    `urlsplit` defers host validation to `.hostname`, which raises ValueError
    here — so the uncaught version aborted a token mint with an exception from
    inside a TLS helper. Fail-closed in that nothing was transmitted, and not a
    path anybody chose; a test that merely tolerated the ValueError would be
    pinning an accident. Both copies catch it and fall to the verifying branch.
    """
    assert tls_ctx("https://[not:an:address]/token") is None
    assert tls_ctx("https://[::1") is None


@pytest.mark.parametrize("host", LOCAL + [h for h in MUST_VERIFY if h])
def test_the_two_copies_agree_on_every_host(host):
    """The mirror, checked as a mirror.

    fabric-target ships as its own distributable and cannot import the agent's
    copy, so the helper is duplicated on purpose — and both fail-opens above
    existed in both copies, verbatim. This is what makes a one-sided repair
    fail the build rather than leaving one credential boundary behind.
    """
    assert tls_scope.is_local_host(host) is fabric_target._is_local_host(host), host


def test_the_table_exercises_both_branches():
    """Non-vacuity, the way this repo's other checkers assert it.

    Every test above would pass against a helper that always returned the same
    answer if the table only held one kind of host. This fails if either side
    of the boundary stops being exercised, or if the two lists ever overlap —
    which would make one of them assert nothing.
    """
    assert len(LOCAL) >= 10 and len([h for h in MUST_VERIFY if h]) >= 10
    assert not set(LOCAL) & {h for h in MUST_VERIFY if h}
    assert _unverified(tls_scope.tls_context("https://entra-emulator:8443/t"))
    assert tls_scope.tls_context("https://api.fabric.microsoft.com/v1") is None
