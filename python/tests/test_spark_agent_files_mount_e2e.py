"""The Files mount: containment, write-back, per-statement refresh, bind refusal.

The unit tests in test_spark_agent_files_mount.py prove `_under_mount` refuses
traversal. This file drives the REAL sync/flush/refresh against a fake
OneLake listing: a `..` entry must not write outside the mount, a local
notebook write must `put` back, an upload after bind must appear on the next
refresh, and a second bind of a different lakehouse must be refused with the
first session's files left intact (docs/37 §2).
"""

import os
import sys
import threading
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
    puts = []

    def put(path, content, overwrite=True):
        puts.append((path, content, overwrite))

    fs = types.SimpleNamespace(
        ls=lambda prefix: entries_by_prefix.get(prefix, []),
        read=lambda path: payload,
        put=put,
        puts=puts,
    )
    module = types.ModuleType("notebookutils")
    module.fs = fs
    monkeypatch.setitem(sys.modules, "notebookutils", module)
    return fs


class FakeFS:
    """Mutable OneLake Files/ tree: ls/read/put from a dict of blobs."""

    def __init__(self):
        self.blobs = {}

    def ls(self, prefix):
        prefix = prefix.rstrip("/")
        found = {}
        for path, data in self.blobs.items():
            if path == prefix or not path.startswith(prefix + "/"):
                continue
            rest = path[len(prefix) + 1:]
            name = rest.split("/", 1)[0]
            child = f"{prefix}/{name}"
            is_dir = "/" in rest
            if is_dir:
                found[name] = Entry(child, name, is_dir=True)
            elif name not in found:
                found[name] = Entry(child, name, size=len(data))
        return list(found.values())

    def read(self, path):
        return self.blobs[path]

    def put(self, path, content, overwrite=True):
        data = content.encode() if isinstance(content, str) else content
        self.blobs[path] = data


def install_tree(monkeypatch, tree: FakeFS):
    module = types.ModuleType("notebookutils")
    module.fs = tree
    monkeypatch.setitem(sys.modules, "notebookutils", module)
    return tree


@pytest.fixture
def mount(tmp_path, monkeypatch):
    root = tmp_path / "lakehouse" / "default" / "Files"
    root.mkdir(parents=True)
    monkeypatch.setattr(files_mount, "MOUNT_ROOT", str(root))
    monkeypatch.setattr(files_mount, "_state", {
        "workspace": None, "lakehouse": None, "seen": {},
    })
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


def test_a_second_bind_of_a_different_lakehouse_is_refused(mount, monkeypatch):
    """docs/37 §2c: switching the mount would silently replace the first
    session's files with the second's. Refuse, and leave the first intact."""
    base_a = "abfss://ws@onelake.dfs.fabric.microsoft.com/lh-a/Files"
    base_b = "abfss://ws@onelake.dfs.fabric.microsoft.com/lh-b/Files"
    tree = FakeFS()
    tree.blobs[f"{base_a}/a.txt"] = b"from-a"
    tree.blobs[f"{base_b}/b.txt"] = b"from-b"
    install_tree(monkeypatch, tree)

    first = files_mount.sync("ws", "lh-a")
    assert first["mounted"] is True
    assert (mount / "a.txt").read_bytes() == b"from-a"

    second = files_mount.sync("ws", "lh-b")
    assert second["mounted"] is False
    assert "lh-a" in second["error"] and "lh-b" in second["error"]
    assert "docs/37" in second["error"]
    assert (mount / "a.txt").read_bytes() == b"from-a"
    assert not (mount / "b.txt").exists()


def test_rebinding_the_same_lakehouse_is_a_re_pull(mount, monkeypatch):
    base = "abfss://ws@onelake.dfs.fabric.microsoft.com/lh/Files"
    tree = FakeFS()
    tree.blobs[f"{base}/contract.json"] = b"v1"
    install_tree(monkeypatch, tree)

    files_mount.sync("ws", "lh")
    tree.blobs[f"{base}/contract.json"] = b"v2-longer"
    again = files_mount.sync("ws", "lh")

    assert again["mounted"] is True
    assert (mount / "contract.json").read_bytes() == b"v2-longer"


def test_local_writes_flush_to_onelake_on_refresh(mount, monkeypatch):
    """docs/37 §2a: a notebook write to the mount must reach OneLake."""
    base = "abfss://ws@onelake.dfs.fabric.microsoft.com/lh/Files"
    tree = FakeFS()
    tree.blobs[f"{base}/seed.txt"] = b"seed"
    install_tree(monkeypatch, tree)

    files_mount.sync("ws", "lh")
    (mount / "nested").mkdir()
    (mount / "nested" / "out.txt").write_bytes(b"from-notebook")

    out = files_mount.refresh()
    assert out["refreshed"] is True
    assert out["flushed"] == 1
    assert tree.blobs[f"{base}/nested/out.txt"] == b"from-notebook"


def test_unchanged_files_are_not_flushed_again(mount, monkeypatch):
    base = "abfss://ws@onelake.dfs.fabric.microsoft.com/lh/Files"
    tree = FakeFS()
    tree.blobs[f"{base}/seed.txt"] = b"seed"
    install_tree(monkeypatch, tree)

    files_mount.sync("ws", "lh")
    first = files_mount.flush()
    second = files_mount.flush()
    assert first["flushed"] == 0
    assert second["flushed"] == 0


def test_onelake_uploads_appear_on_the_next_refresh(mount, monkeypatch):
    """docs/37 §2b: fresh at every statement, not only at the next bind."""
    base = "abfss://ws@onelake.dfs.fabric.microsoft.com/lh/Files"
    tree = FakeFS()
    tree.blobs[f"{base}/already.txt"] = b"old"
    install_tree(monkeypatch, tree)

    files_mount.sync("ws", "lh")
    assert not (mount / "late.txt").exists()

    tree.blobs[f"{base}/late.txt"] = b"uploaded-after-bind"
    files_mount.refresh()
    assert (mount / "late.txt").read_bytes() == b"uploaded-after-bind"


def test_refresh_without_a_mount_is_a_noop(mount):
    assert files_mount.refresh() == {"refreshed": False}
    assert files_mount.flush() == {"flushed": 0, "flush_failed": 0}


def test_flush_does_not_write_outside_the_mount(mount, tmp_path, monkeypatch):
    """A local path that realpath-escapes the mount is not put to OneLake."""
    base = "abfss://ws@onelake.dfs.fabric.microsoft.com/lh/Files"
    tree = FakeFS()
    tree.blobs[f"{base}/ok.txt"] = b"ok"
    install_tree(monkeypatch, tree)
    files_mount.sync("ws", "lh")

    outside = tmp_path / "outside.txt"
    outside.write_bytes(b"secret")
    link = mount / "escape"
    os.symlink(outside, link)

    files_mount.flush()
    assert f"{base}/escape" not in tree.blobs
    assert outside.read_bytes() == b"secret"


def test_a_failed_put_is_counted_not_raised(mount, monkeypatch):
    base = "abfss://ws@onelake.dfs.fabric.microsoft.com/lh/Files"
    tree = FakeFS()
    tree.blobs[f"{base}/ok.txt"] = b"ok"
    install_tree(monkeypatch, tree)
    files_mount.sync("ws", "lh")

    def boom(self, path, content, overwrite=True):
        raise RuntimeError("nope")

    monkeypatch.setattr(FakeFS, "put", boom)
    (mount / "new.txt").write_bytes(b"local")
    out = files_mount.flush()
    assert out["flushed"] == 0
    assert out["flush_failed"] == 1


def test_unavailable_notebookutils_is_reported(mount, monkeypatch):
    monkeypatch.setattr(files_mount, "_import_fs", lambda: None)
    assert files_mount.sync("ws", "lh") == {
        "mounted": False, "error": "notebookutils unavailable",
    }
    assert files_mount.refresh()["refreshed"] is False
    assert files_mount.flush()["flushed"] == 0


def test_flush_after_bind_without_fs_is_reported(mount, monkeypatch):
    """A bind succeeded; the next statement must not crash if fs vanished."""
    base = "abfss://ws@onelake.dfs.fabric.microsoft.com/lh/Files"
    tree = FakeFS()
    tree.blobs[f"{base}/ok.txt"] = b"ok"
    install_tree(monkeypatch, tree)
    files_mount.sync("ws", "lh")
    monkeypatch.setattr(files_mount, "_import_fs", lambda: None)
    assert files_mount.flush()["error"] == "notebookutils unavailable"
    assert files_mount.refresh() == {
        "refreshed": False, "error": "notebookutils unavailable",
    }


def test_a_missing_files_area_still_mounts(mount, monkeypatch):
    """A fresh lakehouse has no Files/ yet; that must not fail the bind."""
    def boom(_prefix):
        raise FileNotFoundError("no Files")

    fs = types.SimpleNamespace(ls=boom, read=lambda _p: b"", put=lambda *_a, **_k: None)
    module = types.ModuleType("notebookutils")
    module.fs = fs
    monkeypatch.setitem(sys.modules, "notebookutils", module)
    out = files_mount.sync("ws", "lh")
    assert out["mounted"] is True
    assert out["copied"] == 0


def test_a_str_payload_is_encoded(mount, monkeypatch):
    base = "abfss://ws@onelake.dfs.fabric.microsoft.com/lh/Files"
    install_fake_fs(monkeypatch, {
        base: [Entry(f"{base}/note.txt", "note.txt", size=5)],
    }, payload="hello")
    files_mount.sync("ws", "lh")
    assert (mount / "note.txt").read_bytes() == b"hello"


def test_a_failed_copy_is_counted_not_raised(mount, monkeypatch):
    base = "abfss://ws@onelake.dfs.fabric.microsoft.com/lh/Files"
    fs = install_fake_fs(monkeypatch, {
        base: [Entry(f"{base}/bad.txt", "bad.txt", size=5)],
    })
    fs.read = lambda _p: (_ for _ in ()).throw(RuntimeError("read failed"))
    out = files_mount.sync("ws", "lh")
    assert out["copied"] == 0
    assert out["failed"] == 1


def test_snapshot_of_a_missing_mount_is_empty(tmp_path, monkeypatch):
    monkeypatch.setattr(files_mount, "MOUNT_ROOT", str(tmp_path / "gone"))
    monkeypatch.setattr(files_mount, "_state", {
        "workspace": None, "lakehouse": None, "seen": {},
    })
    assert files_mount._snapshot() == {}


def test_the_agent_refreshes_the_mount_around_statements():
    """The hook, not the algorithm: if this call disappears, 2a/2b are
    implemented in files_mount.py and never reached from a notebook cell."""
    src = (Path(__file__).resolve().parents[1] / "spark_agent" / "agent.py").read_text()
    statements = src.split('if self.path == "/statements":', 1)[1]
    statements = statements.split("elif self.path ==", 1)[0]
    assert "files_mount.refresh()" in statements
    assert "files_mount.flush()" in statements
    close = src.split('elif self.path == "/close":', 1)[1]
    close = close.split("else:", 1)[0]
    assert "files_mount.flush()" in close


def test_hold_blocks_refresh_until_released(mount, monkeypatch):
    """spark-submit holds this lock; a statement refresh must wait, not tear."""
    _assert_hold_blocks(mount, monkeypatch, lambda: files_mount.refresh())


def test_hold_blocks_flush_until_released(mount, monkeypatch):
    """/close flushes; that must wait too, or it rewrites Files/ under submit."""
    _assert_hold_blocks(mount, monkeypatch, lambda: files_mount.flush())


def test_hold_blocks_sync_until_released(mount, monkeypatch):
    """/mount during a submit must wait rather than replace the tree being read."""
    _assert_hold_blocks(mount, monkeypatch, lambda: files_mount.sync("ws", "lh"))


def _assert_hold_blocks(_mount, monkeypatch, op):
    base = "abfss://ws@onelake.dfs.fabric.microsoft.com/lh/Files"
    tree = FakeFS()
    tree.blobs[f"{base}/ok.txt"] = b"ok"
    install_tree(monkeypatch, tree)
    files_mount.sync("ws", "lh")

    entered = threading.Event()
    release = threading.Event()
    finished = []

    def holder():
        with files_mount.hold():
            entered.set()
            release.wait(2)

    def mutator():
        entered.wait(2)
        op()
        finished.append(True)

    t1 = threading.Thread(target=holder)
    t2 = threading.Thread(target=mutator)
    t1.start()
    t2.start()
    assert entered.wait(1)
    t2.join(0.15)
    assert t2.is_alive() and finished == [], "mount mutation ran while hold() was held"
    release.set()
    t1.join(2)
    t2.join(2)
    assert finished == [True]


def test_concurrent_refresh_and_flush_leave_a_coherent_seen(mount, monkeypatch):
    base = "abfss://ws@onelake.dfs.fabric.microsoft.com/lh/Files"
    tree = FakeFS()
    tree.blobs[f"{base}/ok.txt"] = b"ok"
    install_tree(monkeypatch, tree)
    files_mount.sync("ws", "lh")
    (mount / "local.txt").write_bytes(b"x")

    errors = []

    def hammer():
        try:
            for _ in range(15):
                files_mount.refresh()
                files_mount.flush()
        except Exception as exc:  # noqa: BLE001 - collect, then assert empty
            errors.append(exc)

    threads = [threading.Thread(target=hammer) for _ in range(4)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert errors == []
    with files_mount.hold():
        assert files_mount._state["seen"] == files_mount._snapshot()
