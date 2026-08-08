"""The install-retry contract, driven by pytest as well as by `make check`.

`scripts/check_entra_install.py` asserts the whole contract already. This exists
because a check that only ever runs from a Makefile earns no coverage and can
rot between the two gates — the repo's own convention is a `check_*.py` in
scripts/ with a `test_check_*.py` here, and shipping without one is how
`check_arch_services` came to have "its logic executed by no test at all".

The cases live in the script; these tests drive them, and add the two properties
a reader would otherwise have to take on trust: that the checker FAILS when the
thing it guards is broken, and that its classification is structural rather than
a match against the toolchain's prose.
"""
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_entra_install as c  # noqa: E402


def test_the_contract_holds():
    """Every case in the script, as `make check` runs it."""
    c.main()


def test_the_checker_fails_when_the_retry_is_broken(monkeypatch):
    """A checker that cannot fail proves nothing. Break the classifier — revert
    it to matching Go's prose, which is exactly the regression the structural
    version exists to prevent — and the check must notice."""
    monkeypatch.setattr(c.ei, "_is_transient",
                        lambda output, version: "loading deprecation" in output.lower())
    with pytest.raises(c.RetryContractError):
        c.main()


def test_a_broken_pin_is_not_retried():
    """The property that keeps a retry from becoming a way of not noticing: a
    failure naming the version being installed is real, and must fail on the
    first attempt rather than after three sleeps."""
    assert c.ei._is_transient(c.BROKEN_PIN, "v0.3.0") is False


def test_the_window_is_recognised_by_structure_not_wording():
    """The same failure, reworded beyond recognition, is still the window —
    because it names a version other than the pin, which is true whatever
    words the toolchain wraps around it."""
    assert c.ei._is_transient(c.SUMDB_LAG, "v0.3.0") is True
    assert c.ei._is_transient(c.REWORDED, "v0.3.0") is True


def test_an_unclassifiable_module_failure_says_so():
    """If a future release stops naming versions, the retry silently stops
    firing. The note is what makes that diagnosable instead of invisible."""
    assert "has stopped working" in c.ei._unclassified_note(c.UNCLASSIFIED, "v0.3.0")
    # ...and stays quiet on failures that WERE classified, or it is noise on
    # every genuinely broken pin.
    assert c.ei._unclassified_note(c.BROKEN_PIN, "v0.3.0") == ""


def test_the_version_is_read_from_go_mod():
    """The pin is derived, not declared in nine places with a comment asking a
    human to keep them in step."""
    assert c.ei.module_version(c.ei.ENTRA_MODULE).startswith("v")
    with pytest.raises(RuntimeError):
        c.ei.module_version("example.com/not/pinned")
