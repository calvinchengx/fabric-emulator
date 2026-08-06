"""The mutation must actually apply, or the proof proves nothing.

`scripts/check_kind_exhaustiveness.py` declares an event kind nothing handles
and requires the portal to stop compiling. Its weak point is not the compiler —
it is the mutation: a regex that silently matches nothing leaves the tree
untouched, the build passes because there was never anything wrong with it, and
the harness reports success. That failure mode is real; it has happened in this
repo with a `perl -0pi` edit that quietly did not apply, and the test that then
passed proved nothing at all.

So these tests assert the mutation lands, that the mutated Go is still something
the GENERATOR can read (a mutation it choked on would fail the harness for the
wrong reason), and that a bus.go it cannot edit stops it loudly.

The compile-failure half needs pnpm, a chromium-free but installed portal, and
several seconds of svelte-check; that is the harness's own job in CI, not a unit
test's.
"""
import importlib.util
import sys
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]


def _load(name: str):
    spec = importlib.util.spec_from_file_location(name, REPO / "scripts" / f"{name}.py")
    assert spec and spec.loader
    mod = importlib.util.module_from_spec(spec)
    sys.modules[name] = mod
    spec.loader.exec_module(mod)
    return mod


chk = _load("check_kind_exhaustiveness")
gen = _load("gen_event_kinds")

BUS = (REPO / "internal" / "store" / "bus.go").read_text(encoding="utf-8")


def test_the_real_bus_does_not_already_contain_the_ghost():
    """Otherwise a previous run leaked its mutation and every case below lies."""
    assert "KindGhost" not in BUS
    assert '"ghost"' not in BUS


@pytest.mark.parametrize("viewable", [True, False])
def test_the_kind_is_declared_and_reaches_allkinds(viewable):
    out = chk.mutate(BUS, viewable=viewable)
    assert 'KindGhost = "ghost"' in out
    all_body = out.split("var AllKinds = []string{")[1].split("}")[0]
    assert "KindGhost" in all_body


def test_viewable_decides_whether_it_is_a_filter():
    """The two mutations must differ, or one of the two proofs is a duplicate."""
    view_body = lambda src: src.split("var ViewKinds = []string{")[1].split("}")[0]  # noqa: E731
    assert "KindGhost" in view_body(chk.mutate(BUS, viewable=True))
    assert "KindGhost" not in view_body(chk.mutate(BUS, viewable=False))


@pytest.mark.parametrize("viewable", [True, False])
def test_the_generator_can_still_read_the_mutated_bus(tmp_path, monkeypatch, viewable):
    """A mutation the generator chokes on would fail the harness for the wrong
    reason — and 'the build broke' would then mean nothing about exhaustiveness.
    """
    fake = tmp_path / "bus.go"
    fake.write_text(chk.mutate(BUS, viewable=viewable), encoding="utf-8")
    monkeypatch.setattr(gen, "BUS", fake)
    out = gen.render()
    assert "'ghost'," in out
    # The union is what the portal's switch is checked against; if the kind did
    # not reach it, nothing downstream could fail to compile.
    assert "'ghost'" in out.split("export const EVENT_KINDS = [")[1].split("]")[0]


def test_a_bus_without_the_anchor_constant_stops_the_harness():
    """A silent no-op is the one outcome worse than a failure."""
    with pytest.raises(SystemExit) as e:
        chk.mutate(BUS.replace('KindDropped  = "dropped"', 'KindLost = "lost"'),
                   viewable=True)
    assert "KindDropped" in str(e.value)


def test_a_bus_without_the_slice_stops_the_harness():
    without = BUS.replace("var ViewKinds = []string{", "var ViewKinds = []string{ /* moved */")
    with pytest.raises(SystemExit) as e:
        chk.mutate(without, viewable=True)
    assert "ViewKinds" in str(e.value)


def test_the_inserted_line_says_it_is_not_real():
    """A leaked mutation has to be obvious in a diff, not look half-landed."""
    assert "NOT REAL" in chk.CONST_LINE
    assert "check_kind_exhaustiveness" in chk.CONST_LINE
