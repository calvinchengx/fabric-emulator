r"""The check that catches an override nothing reads, itself unread until now.

check_endpoint_env_names exists because three callers kept setting
`FABRIC_REST_URL` and `KV_URL` after endpoint resolution moved into
fabric-target, which reads neither. THE DEFAULTS ARE WHY NOBODY NOTICED: they
are exactly the ports CI publishes, so an override that did nothing looked like
one that worked. It only broke on a remapped stack -- `docs/demo/flow-override.yml`
asked for 9843 and then talked to whatever answered on 9443.

The checker had no tests, and its two extractors are regexes over other files'
source. Both fail the same silent way: match nothing, report every caller
clean, and the build stays green while the guard has stopped guarding.

So the tests that matter are the ones proving each half still finds things --
`accepted()` really reading fabric-target's names, and `set_by()` really seeing
what a caller sets -- plus the end-to-end failure on a planted bad override.
A checker whose extractor returns an empty set would pass a naive round-trip
test, and fails these.
"""
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_endpoint_env_names as c  # noqa: E402

# The shape fabric-target's source actually has: a tuple of aliases per
# endpoint, plus single-name reads.
TARGET_SRC = '''
def resolve():
    fabric = _env_any(("FABRIC_EMULATOR_URL", "FABRIC_URL"))
    entra = _env_any(("ENTRA_EMULATOR_URL", "ENTRA_URL"))
    vault = _env_any((
        "VAULT_EMULATOR_URL",
        "AZURE_KEY_VAULT_URL",
    ))
    mode = _env("FABRIC_TARGET")
    return fabric, entra, vault, mode
'''


@pytest.fixture
def tree(tmp_path, monkeypatch):
    """A miniature repo: fabric-target's source plus one caller."""
    target = tmp_path / "fabric_target.py"
    target.write_text(TARGET_SRC)
    monkeypatch.setattr(c, "ROOT", tmp_path)
    monkeypatch.setattr(c, "TARGET", target)
    return tmp_path


# --- accepted(): reading the names out of fabric-target ------------------------

def test_accepted_reads_every_alias_in_a_tuple(tree):
    """Both halves of `_env_any(("A", "B"))`, or the second alias is unusable."""
    got = c.accepted()
    assert {"FABRIC_EMULATOR_URL", "FABRIC_URL"} <= got
    assert {"ENTRA_EMULATOR_URL", "ENTRA_URL"} <= got


def test_accepted_reads_a_tuple_split_across_lines(tree):
    """The vault pair is wrapped in the real source; a non-DOTALL regex misses it."""
    assert {"VAULT_EMULATOR_URL", "AZURE_KEY_VAULT_URL"} <= c.accepted()


def test_accepted_reads_a_single_name_read(tree):
    assert "FABRIC_TARGET" in c.accepted()


def test_accepted_is_not_empty_on_the_real_source():
    """The extractor against the tree it actually guards.

    An empty set here is the silent failure: every override would then be
    reported as unread, or -- with COMMON_OWNED absorbing them -- nothing would
    be checked at all.
    """
    assert c.TARGET.is_file(), f"{c.TARGET} moved; the checker reads nothing"
    assert len(c.accepted()) >= 4


# --- set_by(): reading what a caller sets --------------------------------------

def test_set_by_finds_endpoint_shaped_names(tree):
    (tree / "caller.py").write_text(
        'env = {"FABRIC_URL": u, "TDS_SERVER": s, "SPARK_REMOTE": r}\n'
    )
    assert c.set_by(pathlib.Path("caller.py")) == {"FABRIC_URL", "TDS_SERVER", "SPARK_REMOTE"}


def test_set_by_ignores_names_that_are_not_endpoints(tree):
    """A caller may set anything; only endpoint-shaped names are this check's business."""
    (tree / "caller.py").write_text('env = {"WORKSPACE": w, "FABRIC_URL": u, "TIMEOUT": t}\n')
    assert c.set_by(pathlib.Path("caller.py")) == {"FABRIC_URL"}


# --- main(): the end-to-end verdict --------------------------------------------

def test_an_override_nothing_reads_fails_and_is_named(tree, capsys):
    """The original defect, planted: FABRIC_REST_URL is read by nothing."""
    (tree / "caller.py").write_text('env = {"FABRIC_REST_URL": u}\n')
    with pytest.MonkeyPatch.context() as mp:
        mp.setattr(c, "CALLERS", [pathlib.Path("caller.py")])
        assert c.main() == 1
    err = capsys.readouterr().err
    assert "FABRIC_REST_URL" in err
    assert "silently fall back" in err
    assert "FABRIC_EMULATOR_URL" in err, "the report must list what IS accepted"


def test_an_override_fabric_target_reads_passes(tree, capsys):
    (tree / "caller.py").write_text('env = {"FABRIC_EMULATOR_URL": u, "ENTRA_URL": e}\n')
    with pytest.MonkeyPatch.context() as mp:
        mp.setattr(c, "CALLERS", [pathlib.Path("caller.py")])
        assert c.main() == 0
    assert "every endpoint override names a variable" in capsys.readouterr().out


def test_a_common_owned_name_is_allowed(tree):
    """These are read by examples/contoso-fixtures/common.py, not fabric-target.

    Without the allowance the check would report working configuration, which
    is the false positive that gets a checker muted.
    """
    (tree / "caller.py").write_text('env = {"TDS_SERVER": s, "KV_INTERNAL_URL": k}\n')
    with pytest.MonkeyPatch.context() as mp:
        mp.setattr(c, "CALLERS", [pathlib.Path("caller.py")])
        assert c.main() == 0


def test_every_common_owned_entry_can_actually_be_reached():
    """An entry set_by() would never surface is a dead line in the allow-list.

    COMMON_OWNED only does work for names that are endpoint-shaped, because
    those are the only ones that reach the membership test at all. An entry
    failing ENDPOINTISH is not wrong so much as inert — it reads as a
    deliberate allowance while allowing nothing — so it should be deleted or
    the pattern widened, and this says which entries are in that state.
    """
    reachable = {n for n in c.COMMON_OWNED if c.ENDPOINTISH.match(n)}
    assert reachable, "no COMMON_OWNED entry is reachable; the allow-list is inert"

    # The four below are NOT endpoint-shaped, so set_by() never surfaces them
    # and the allowance never fires. They are pinned rather than deleted:
    # removing an entry from somebody's allow-list on the strength of a regex
    # is a change with its own argument to make. Pinning means a FIFTH inert
    # entry fails here, so the list cannot quietly fill with dead lines.
    known_inert = {"DEFINITIONS", "GOLD_PROJECT", "PIPELINE_STATE", "WORKSPACE_NAME"}
    inert = {n for n in c.COMMON_OWNED if not c.ENDPOINTISH.match(n)}
    assert inert == known_inert, (
        f"the inert set moved: {sorted(inert)}. A new entry here allows nothing "
        f"— widen ENDPOINTISH, or drop the entry."
    )


def test_the_real_tree_passes():
    """The callers as they actually stand."""
    assert c.main() == 0
