"""The second-source check, exercised — including its own failure modes.

Contract 8's expectations are a transcription of somebody else's contract, and
this guard holds them against sources outside the repository so a stale one
cannot pass quietly. A guard against silent staleness that is itself untested
is the same defect one level up, which is why the coverage gate refused the
first version of this script: 70 statements, none of them executed.

What matters here is not that it passes on the tree as it stands — that is one
data point and it would keep saying so after the logic rotted. It is that each
disagreement it claims to catch actually makes it fail.
"""
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_refusal_expectations as c  # noqa: E402

ENUM = '''
class StorageErrorCode(str, Enum):
    directory_not_empty = "DirectoryNotEmpty"
    blob_not_found = "BlobNotFound"
'''


def _live(tmp_path, mapping):
    p = tmp_path / "live.py"
    body = ",\n".join(f'    {k!r}: {v!r}' for k, v in mapping.items())
    p.write_text(f"REFUSAL_EXPECTATIONS = {{\n{body}\n}}\n", encoding="utf-8")
    return p


def _vendored(tmp_path, text=ENUM):
    p = tmp_path / "models.py"
    p.write_text(text, encoding="utf-8")
    return p


@pytest.fixture
def wired(tmp_path, monkeypatch):
    """Point the module at fixtures rather than the repository's own files."""
    def _go(mapping, enum=ENUM):
        monkeypatch.setattr(c, "LIVE", _live(tmp_path, mapping))
        monkeypatch.setattr(c, "VENDORED", _vendored(tmp_path, enum))
    return _go


def test_it_passes_when_both_sources_agree(wired, monkeypatch, capsys):
    monkeypatch.setattr(c, "POSIX", True)
    wired({"dfs-delete-non-empty-directory": "409 DirectoryNotEmpty",
           "rm-non-empty-without-recurse": c.posix_rmdir_errno()})
    assert c.main() == 0


def test_a_code_microsoft_does_not_define_fails(wired, capsys):
    """The staleness this exists for: a renamed or retired code."""
    wired({"dfs-delete-non-empty-directory": "409 DirectoryWasNotEmpty"})
    assert c.main() == 1
    assert "not among the" in capsys.readouterr().err


def test_an_errno_cpython_does_not_raise_fails(wired, monkeypatch, capsys):
    """Forced onto the POSIX path so the comparison is exercised everywhere.

    `main()` SKIPS this corroboration off POSIX, which is deliberate — the
    errno differs on Windows. Without forcing it this test asserted a failure
    the Windows runner never produced, and it passed on two platforms of three
    while proving nothing on the third.

    Forced through the module's own flag, NOT by patching `os.name`: pathlib
    reads that to choose PosixPath over WindowsPath, and patching it made every
    `Path()` on the Windows runner raise NotImplementedError."""
    monkeypatch.setattr(c, "POSIX", True)
    wired({"rm-non-empty-without-recurse": "OSError/EACCES"})
    assert c.main() == 1
    assert "CPython raises" in capsys.readouterr().err


def test_a_case_with_no_declared_source_fails(wired, capsys):
    """A new case must be corroborated or declared, never silently graded."""
    wired({"something-new": "409 Whatever"})
    assert c.main() == 1
    assert "no source declared" in capsys.readouterr().err


def test_a_declared_single_source_is_allowed_through(wired):
    wired({"mv-onto-existing-without-overwrite": "FileExistsError"})
    assert c.main() == 0


def test_a_second_source_that_lost_the_enum_is_not_silently_empty(wired, capsys):
    """If the vendored file stops carrying StorageErrorCode, the check must say
    so — an empty code set would otherwise make every expectation unverifiable
    while still reporting a comparison."""
    wired({"dfs-delete-non-empty-directory": "409 DirectoryNotEmpty"},
          enum="class RenamedAway(str, Enum):\n    x = 'y'\n")
    assert c.main() == 1
    assert "no codes parsed" in capsys.readouterr().err


def test_a_missing_second_source_is_refused_rather_than_skipped(tmp_path, monkeypatch):
    monkeypatch.setattr(c, "LIVE", _live(
        tmp_path, {"dfs-delete-non-empty-directory": "409 DirectoryNotEmpty"}))
    monkeypatch.setattr(c, "VENDORED", tmp_path / "absent.py")
    with pytest.raises(SystemExit):
        c.main()


def test_cpython_really_refuses_a_non_empty_directory():
    """The corroboration itself, asserted: if this ever returns "no error" the
    POSIX check silently compares against nothing."""
    got = c.posix_rmdir_errno()
    assert got.startswith("OSError/"), got
    assert got != "no error"


def test_the_expectations_parse_from_the_real_file():
    """The ast path against the tree's own live.py, not a fixture."""
    exp = c.expectations()
    assert exp, "REFUSAL_EXPECTATIONS parsed empty from the real live.py"
    assert "dfs-delete-non-empty-directory" in exp


def test_a_live_file_without_the_map_is_refused(tmp_path, monkeypatch):
    """Parsing rather than importing means the name can simply be absent — and
    that has to be loud, not an empty dict that grades nothing."""
    p = tmp_path / "live.py"
    p.write_text("SOMETHING_ELSE = {}\n", encoding="utf-8")
    monkeypatch.setattr(c, "LIVE", p)
    with pytest.raises(SystemExit):
        c.expectations()


def test_the_posix_corroboration_is_skipped_not_faked_off_posix(wired, monkeypatch, capsys):
    """On Windows the errno differs, so the check reports the corroboration as
    SKIPPED. It must not quietly pass as though CPython had agreed."""
    wired({"rm-non-empty-without-recurse": "OSError/ENOTEMPTY"})
    monkeypatch.setattr(c, "POSIX", False)
    assert c.main() == 0
    assert "corroboration skipped" in capsys.readouterr().out
