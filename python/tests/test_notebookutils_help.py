"""`help()` — the discovery mechanism Fabric's own fs page opens by documenting.

It was on no module here until the vendored stubs were held beside the
documentation and turned "we transcribed carefully" into a list. The
transcription could not have yielded it: it is prose on the page rather than a
row in the method table.

THE PROPERTY UNDER TEST IS THAT IT IS DERIVED. A hand-written help table would
be a second description of the same surface, and this repository keeps finding
that exact defect — so the assertions are about help() tracking the module
rather than about any particular wording.
"""
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

from notebookutils import (  # noqa: E402
    _help,
    credentials,
    fs,
    lakehouse,
    notebook,
    runtime,
    session,
    udf,
    variableLibrary,
)

# Every module Microsoft's stub gives a `help`, and the two it does not.
WITH_HELP = [fs, credentials, lakehouse, notebook, runtime, session, udf]


@pytest.mark.parametrize("module", WITH_HELP, ids=lambda m: m.__name__)
def test_every_module_the_stub_gives_help_has_one(module):
    assert callable(module.help)


@pytest.mark.parametrize("module", WITH_HELP, ids=lambda m: m.__name__)
def test_each_help_describes_its_own_module(module, capsys):
    """Each `help` resolves `_sys.modules[__name__]`, which is the same line in
    eight files — so the failure mode is one of them naming another module and
    every notebook getting the wrong listing. Calling each is what catches it.
    """
    module.help()
    printed = capsys.readouterr().out
    assert printed.startswith(module.__name__), printed.split("\n", 1)[0]


def test_variable_library_has_no_help_because_the_stub_gives_it_none():
    """Inventing the missing half would be this shim's shape, not Fabric's.

    `variableLibrary` carries `getHelpString` and no `help` in Microsoft's
    stubs. The asymmetry is odd, and copying it is the point: contract 2 grades
    the surface a framework introspects, not the one we would have designed.
    """
    assert not hasattr(variableLibrary, "help")
    assert callable(variableLibrary.getHelpString)


def test_the_listing_is_derived_from_the_module(monkeypatch):
    """A method added to the module appears without anyone editing help text."""
    def freshly_added(alpha, beta=2):
        """A method nobody wrote help for."""

    freshly_added.__module__ = fs.__name__
    monkeypatch.setattr(fs, "freshly_added", freshly_added, raising=False)
    listing = _help.help_string(fs)
    assert "freshly_added(alpha, beta=2)" in listing
    assert "A method nobody wrote help for." in listing


def test_the_listing_names_real_methods_with_their_real_parameters():
    listing = _help.help_string(fs)
    # The parameter NAMES are contract 2's subject, so help must show them
    # rather than a prose summary that could drift from the signature.
    assert "rm(path, recurse=False)" in listing
    assert "put(file, content, overwrite=False)" in listing


def test_one_method_gives_its_full_documentation():
    one = _help.help_string(fs, "rm")
    assert one.startswith("rm(path, recurse=False)")
    # The whole docstring, not the summary line: this is the "tell me about
    # this method" call.
    assert "recurse" in one and "ADLS" in one


def test_an_unknown_method_lists_what_there_is():
    """The useful answer to a typo is the alternatives, not an exception —
    this is a discovery call, and raising would end the discovery."""
    answer = _help.help_string(fs, "remove")
    assert "no method 'remove'" in answer
    assert "rm" in answer


def test_help_prints_and_returns_none(capsys):
    """Fabric's is an output call. A caller wanting the string has
    getHelpString, which is why both exist."""
    assert fs.help() is None
    assert "notebookutils.fs" in capsys.readouterr().out


def test_help_for_one_method_prints_it(capsys):
    fs.help("rm")
    assert "rm(path, recurse=False)" in capsys.readouterr().out


def test_udf_help_takes_method_not_method_name():
    """Microsoft's stub spells this one parameter differently from every other
    module, and a caller passing it by keyword is exactly why parameter names
    are contract 2's subject. Tidying it would be a divergence."""
    import inspect

    assert list(inspect.signature(udf.help).parameters) == ["method"]
    assert list(inspect.signature(fs.help).parameters) == ["method_name"]


def test_gethelpstring_returns_rather_than_prints(capsys):
    text = udf.getHelpString()
    assert "notebookutils.udf" in text
    assert capsys.readouterr().out == ""


def test_gethelpstring_selects_one_function():
    assert udf.getHelpString("getFunctions").startswith("getFunctions(")


def test_a_module_without_a_docstring_still_lists(monkeypatch):
    import types

    module = types.ModuleType("bare")

    def only(x):
        """Does one thing."""

    only.__module__ = "bare"
    module.only = only
    listing = _help.help_string(module)
    assert listing.startswith("bare")
    assert "only(x)" in listing


def test_help_does_not_list_imported_names():
    """`_help` imports `inspect`, and modules import each other. A listing that
    showed another module's functions would describe the wrong surface."""
    listing = _help.help_string(lakehouse)
    assert "getToken" not in listing   # credentials', not lakehouse's
    assert "help_string" not in listing
