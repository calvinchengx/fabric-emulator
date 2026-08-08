"""The capture redactor, driven by pytest as well as by `make check`.

`scripts/check_capture_redaction.py` asserts that a definition captured from a
real tenant is reduced to types and keys before anything is printed — a claim
that matters because this repository is public and so are its Actions logs.

Driven from here too, for the repo's usual reason: a checker that runs only from
a Makefile earns no coverage and can drift silently between the two gates. The
extra properties below are the ones a reader would otherwise take on trust —
that the checker FAILS on a leaky redactor, and on an over-zealous one.
"""
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_capture_redaction as c  # noqa: E402


def test_the_contract_holds():
    """Every case in the script, as `make check` runs it."""
    c.main()


def test_the_checker_fails_on_a_leaky_redactor(monkeypatch):
    """The failure that matters: a redactor passing tenant strings through.
    A check that cannot catch it is worse than none, because it certifies."""
    monkeypatch.setattr(c.cds, "shape", lambda node, key=None: node)
    with pytest.raises(c.RedactionError):
        c.main()


def test_the_checker_fails_on_an_over_zealous_redactor(monkeypatch):
    """The opposite failure, which is quieter: redact the discriminators too and
    the output is perfectly safe and completely useless — nothing is captured."""
    monkeypatch.setattr(c.cds, "KEEP_VALUE_KEYS", set())
    with pytest.raises(c.RedactionError):
        c.main()


@pytest.mark.parametrize("secret", c.SECRETS)
def test_no_tenant_value_survives(secret):
    """Each value individually, so a failure names which one escaped rather
    than only that something did."""
    import json
    assert secret not in json.dumps(c.cds.shape(c.DEFINITION))


def test_discriminators_survive():
    """The capture exists for these: they are Microsoft's vocabulary, not the
    tenant's, and they are what the emulator dispatches on."""
    found = c.cds.discriminators(c.DEFINITION, set())
    assert {"Copy", "Teams", "LakehouseTableSource", "LakehouseTableSink"} <= found
    assert not any(secret in found for secret in c.SECRETS)
