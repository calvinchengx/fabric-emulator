"""sync() itself must not write outside the mount, not merely its helper.

The unit tests in test_spark_agent_files_mount.py prove `_under_mount` refuses
traversal. That is a claim about a function; this is the claim that matters —
drive the REAL sync() against a lakehouse listing that contains `..` entries
and assert the filesystem outside the mount is untouched afterwards.

Written because CodeQL kept reporting py/path-injection after the guard landed:
either its dataflow does not recognise the sanitiser, or containment leaks
somewhere the unit tests do not reach. This test is what tells those apart.
"""

import sys
import types
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))

import files_mount  # noqa: E402


class Entry:
    def __init__(self, path, name, is_dir=False, size=0):
        self.path = path
        self.name = name
        self.isDir = is_dir  # noqa: N815 — mirrors notebookutils' attribute
        self.size = size


def install_fake_fs(monkeypatch, entries_by_prefix, payload=b"pwned"):
    """Install a fake notebookutils.fs that serves the given listing."""
    fs = types.SimpleNamespace(
        ls=lambda prefix: entries_by_prefix.get(prefix, []),
        read=lambda path: payload,
    )
    module = types.ModuleType("notebookutils")
    module.fs = fs
    monkeypatch.setitem(sys.modules, "notebookutils", module)
    return fs


@pytest.fixture
def mount(tmp_path, monkeypatch):
    root = tmp_path / "lakehouse" / "default" / "Files"
    root.mkdir(parents=True)
    monkeypatch.setattr(files_mount, "MOUNT_ROOT", str(root))
    monkeypatch.setattr(files_mount, "_state", {"lakehouse": None})
    return root


def test_traversal_entry_writes_nothing_outside_the_mount(mount, tmp_path, monkeypatch):
    base = "abfss://ws@onelake.dfs.fabric.microsoft.com/lh/Files"
    # A lakehouse whose listing tries to climb out of Files/ three ways.
    install_fake_fs(monkeypatch, {
        base: [
            Entry(f"{base}/../../../escaped.txt", "../../../escaped.txt", size=5),
            Entry("/Files/../../also-escaped.txt", "x", size=5),
            Entry(f"{base}/legit.txt", "legit.txt", size=5),
        ],
    })

    summary = files_mount.sync("ws", "lh")

    # The benign file still mirrors — the guard must not break the feature.
    assert (mount / "legit.txt").read_bytes() == b"pwned"
    assert summary["copied"] == 1
    assert summary["failed"] == 2

    # Nothing was written anywhere above the mount root.
    outside = [
        p for p in tmp_path.rglob("*")
        if p.is_file() and not str(p).startswith(str(mount))
    ]
    assert outside == [], f"sync() wrote outside the mount: {outside}"


def test_traversal_via_directory_recursion_is_contained(mount, tmp_path, monkeypatch):
    """A directory entry escaping the mount must not become a makedirs target."""
    base = "abfss://ws@onelake.dfs.fabric.microsoft.com/lh/Files"
    install_fake_fs(monkeypatch, {
        base: [Entry(f"{base}/../../evil-dir", "../../evil-dir", is_dir=True)],
    })

    files_mount.sync("ws", "lh")

    assert not (tmp_path / "evil-dir").exists()
    assert not (tmp_path / "lakehouse" / "evil-dir").exists()


def test_absolute_entry_name_is_contained(mount, tmp_path, monkeypatch):
    """os.path.join discards the root when the second arg is absolute — the
    classic way a join-based mirror writes to /etc."""
    base = "abfss://ws@onelake.dfs.fabric.microsoft.com/lh/Files"
    target = tmp_path / "absolute-escape.txt"
    install_fake_fs(monkeypatch, {
        base: [Entry(f"/Files/{target}", str(target), size=5)],
    })

    files_mount.sync("ws", "lh")

    assert not target.exists()
