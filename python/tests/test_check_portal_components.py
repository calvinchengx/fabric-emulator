"""Tests for the vendored-portal-component orphan check.

WHAT IS ACTUALLY AT RISK HERE, and it is the same shape as the defect the
checker exists to catch. This guard's failure mode is SILENT: a regex that
stops matching, or a scan root that moves, reports zero orphans -- which is
indistinguishable from a healthy tree and is exactly what a reader wants to
see. So almost every case below drives a tree it MUST refuse, and the clean
tree is the single control. A suite that only fed it good input would stay
green through the deletion of the check.

The load-bearing case is `test_a_component_imported_only_from_inside_ui_is_an_orphan`.
That is the `separator/` shape -- a directory with a real referrer that is
itself dead -- and it is the one a mention-anywhere sweep gets wrong. If one
test here earns its keep, it is that one.

Synthetic trees throughout, with `c.ROOT` moved to tmp_path, rather than the
repo's own portal: the real portal is edited by other work, and a test that
reads it asserts today's component list rather than the checker's behaviour.
The one exception is the last test, which is the control in the other
direction.
"""
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_portal_components as c  # noqa: E402


@pytest.fixture
def portal(tmp_path, monkeypatch):
    """Build a synthetic portal tree and point the checker at it.

    `components` maps a directory name to the files inside it; `app` maps a
    path under portal/src (outside ui/) to its contents. Both default to the
    healthy arrangement, so each test below mutates exactly one thing.
    """

    def build(components=None, app=None):
        components = {"card": {"index.ts": "export { default as Card } from './card.svelte';"}} \
            if components is None else components
        app = {"App.svelte": "import { Card } from '$lib/components/ui/card/index';"} \
            if app is None else app

        ui = tmp_path / c.UI_DIR
        for name, files in components.items():
            (ui / name).mkdir(parents=True, exist_ok=True)
            for fname, body in files.items():
                (ui / name / fname).write_text(body, encoding="utf-8")

        src = tmp_path / c.APP_ROOT
        for rel, body in app.items():
            path = src / rel
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(body, encoding="utf-8")

        monkeypatch.setattr(c, "ROOT", tmp_path)
        return tmp_path

    return build


# --- the control, and the orphan it must find -----------------------------


def test_an_imported_component_is_not_an_orphan(portal):
    """The control. Without it the refusals below prove only that it refuses."""
    portal()
    assert c.orphans() == []
    assert c.main(["check_portal_components.py", "--strict"]) == 0


def test_a_component_nothing_imports_is_an_orphan(portal):
    """The `select/` shape: 12 files, referenced by nothing outside itself."""
    portal(components={
        "card": {"index.ts": "export {};"},
        "select": {
            "index.ts": "import Root from './select.svelte';",
            "select.svelte": "<script>let x;</script>",
        },
    })
    assert c.orphans() == ["select"]
    assert c.main(["check_portal_components.py", "--strict"]) == 1


def test_a_component_imported_only_from_inside_ui_is_an_orphan(portal):
    """THE `separator/` SHAPE, and the reason this checker is not a grep.

    separator/ had a referrer for its whole life --
    select/select-separator.svelte -- and select/ was itself unreachable. A
    sweep that accepted any mention would have called separator/ healthy
    before select/ was deleted AND after, because the only thing holding it up
    was the dead directory's own file. Dead code holds dead code alive.
    """
    portal(components={
        "card": {"index.ts": "export {};"},
        "select": {
            "select-separator.svelte":
                "import { Separator } from '$lib/components/ui/separator/index.js';",
        },
        "separator": {"index.ts": "export {};"},
    })
    # BOTH, and in one run: they are dead together, which is the finding.
    assert c.orphans() == ["select", "separator"]


def test_the_orphan_is_named_in_the_strict_message(portal, capsys):
    """A count is not a finding: `9 -> 8` in review names nothing.

    check_route_coverage.py records the same lesson one directory over, where
    storing `registered` as a count meant deleting a served route read as a
    number going down.
    """
    portal(components={"card": {"index.ts": "export {};"},
                       "select": {"index.ts": "export {};"}})
    assert c.main(["check_portal_components.py", "--strict"]) == 1
    out = capsys.readouterr().out
    assert "ui/select/" in out
    assert "ui/card/" not in out


# --- the ways the match could be too loose or too tight -------------------


def test_a_longer_sibling_name_does_not_satisfy_a_shorter_one(portal):
    """Without the trailing lookahead, `select` is satisfied by `select-native`.

    A guard that a DIFFERENT component can satisfy is worse than no guard: it
    reports the tree healthy while the thing it was asked about is dead.
    """
    portal(
        components={"select": {"index.ts": "export {};"},
                    "select-native": {"index.ts": "export {};"}},
        app={"App.svelte":
             "import X from '$lib/components/ui/select-native/index';"},
    )
    assert c.orphans() == ["select"]


@pytest.mark.parametrize("spelling", [
    "import { Card } from '$lib/components/ui/card/index';",
    'import * as Card from "$lib/components/ui/card/index.js";',
    "import { Card } from '../lib/components/ui/card';",
    "const m = await import('./components/ui/card/index.js');",
])
def test_every_spelling_of_the_import_counts(portal, spelling):
    """What is asserted is that the directory is REACHED, not which alias won.

    `$lib`, a relative path and a dynamic import all resolve to the same
    directory, and a checker that recognised only the alias this repo happens
    to use today would turn a refactor into a false finding.
    """
    portal(app={"App.svelte": spelling})
    assert c.orphans() == []


def test_a_barrel_subcomponent_does_not_produce_a_finding(portal):
    """DIRECTORY granularity, which is the decision most likely to be argued.

    card-action.svelte is imported by its own index.ts and by nothing else --
    a barrel publishes a component's whole surface whether or not today's
    pages use every part. A file-level sweep would report it, and a guard
    whose output is argued with instead of fixed stops being run.
    """
    portal(components={"card": {
        "index.ts": "export { default as CardAction } from './card-action.svelte';",
        "card-action.svelte": "<script>let x;</script>",
    }})
    assert c.orphans() == []


def test_a_non_source_file_in_the_app_tree_is_not_read_as_a_referrer(portal):
    """Only .svelte/.ts/.js carry imports; a README naming a component does not.

    The same distinction check_doc_drift.py draws: a path in prose is not a
    path being used, and accepting one would let a stale sentence hold a dead
    directory alive -- this checker's whole subject.
    """
    portal(
        components={"card": {"index.ts": "export {};"}},
        app={"README.md": "We use $lib/components/ui/card/index for the tiles."},
    )
    assert c.orphans() == ["card"]


# --- the sweep that covers nothing ----------------------------------------


def test_no_component_directories_is_itself_a_finding(portal, capsys):
    """A moved `ui/` leaves this reporting zero orphans, which looks healthy."""
    portal(components={}, app={"App.svelte": "// nothing"})
    assert c.main(["check_portal_components.py", "--strict"]) == 1
    assert "passes vacuously" in capsys.readouterr().out


def test_no_application_files_is_itself_a_finding(portal, capsys):
    """And the other direction, which fails the opposite way.

    With no application files every component is unreachable, so this would
    report EVERY directory as an orphan -- a broken scan wearing a finding's
    clothes. Separate message from the one above because the cause is
    different: `ui/` moved, or `portal/src` did.
    """
    portal(components={"card": {"index.ts": "export {};"}}, app={})
    assert c.main(["check_portal_components.py", "--strict"]) == 1
    assert "broken scan" in capsys.readouterr().out


# --- the non-strict mode and the real tree --------------------------------


def test_without_strict_an_orphan_is_reported_but_does_not_fail(portal, capsys):
    """Same contract as every sibling: report at exit 0, refuse at --strict."""
    portal(components={"card": {"index.ts": "export {};"},
                       "select": {"index.ts": "export {};"}})
    assert c.main(["check_portal_components.py"]) == 0
    out = capsys.readouterr().out
    assert "ORPHAN" in out and "select" in out


def test_a_path_inside_the_repo_is_shortened():
    """Forward slashes, so one finding reads the same on all three platforms."""
    assert c.relkey(c.ROOT / "portal" / "src") == "portal/src"


def test_the_real_portal_has_no_orphaned_components():
    """The one case that reads the repository.

    Everything above drives synthetic trees, which proves the checker works
    and says nothing about what is committed. This is the control in the other
    direction -- and it is NOT skipped when the portal is absent, because a
    skip would look identical to a pass on the day somebody moves it.
    """
    assert (c.ROOT / c.UI_DIR).is_dir(), f"{c.UI_DIR} is missing"
    assert c.orphans() == []
