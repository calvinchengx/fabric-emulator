"""The mount mirror must stay inside the mount.

`rel` is derived from a REMOTE listing — the lakehouse decides those names — so
a `..` in an entry name is untrusted input reaching os.path.join. Real Fabric
FUSE-mounts the lakehouse and cannot be escaped this way; this mirror of it
must refuse the same. CodeQL flagged five paths into the writer (py/path-injection).
"""

import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))

import files_mount  # noqa: E402


def test_ordinary_relative_paths_land_under_the_mount(tmp_path, monkeypatch):
    monkeypatch.setattr(files_mount, "MOUNT_ROOT", str(tmp_path))
    root = os.path.realpath(tmp_path)

    for rel in ("contract.json", "nested/dir/data.csv", "./also-fine.txt"):
        local = files_mount._under_mount(rel)
        assert local is not None, rel
        assert os.path.commonpath([root, local]) == root


def test_traversal_is_refused(tmp_path, monkeypatch):
    monkeypatch.setattr(files_mount, "MOUNT_ROOT", str(tmp_path / "mount"))
    os.makedirs(tmp_path / "mount", exist_ok=True)

    # Each of these would otherwise resolve outside the mount root.
    for rel in (
        "../escaped.txt",
        "../../etc/passwd",
        "nested/../../escaped.txt",
        "/absolute/elsewhere.txt",  # os.path.join discards the root on an absolute arg
    ):
        assert files_mount._under_mount(rel) is None, rel


def test_sibling_directory_sharing_a_prefix_is_refused(tmp_path, monkeypatch):
    """A string-prefix check would accept this; commonpath does not.

    /…/mount-evil starts with /…/mount as text while being a different
    directory, which is exactly the bug a `startswith` guard ships with.
    """
    monkeypatch.setattr(files_mount, "MOUNT_ROOT", str(tmp_path / "mount"))
    os.makedirs(tmp_path / "mount", exist_ok=True)
    os.makedirs(tmp_path / "mount-evil", exist_ok=True)

    assert files_mount._under_mount("../mount-evil/x.txt") is None


def test_symlink_inside_the_mount_is_not_an_escape(tmp_path, monkeypatch):
    """Containment is decided after realpath, so a symlink already in the tree
    cannot be used as the way out."""
    mount = tmp_path / "mount"
    outside = tmp_path / "outside"
    os.makedirs(mount, exist_ok=True)
    os.makedirs(outside, exist_ok=True)
    os.symlink(outside, mount / "link")
    monkeypatch.setattr(files_mount, "MOUNT_ROOT", str(mount))

    assert files_mount._under_mount("link/x.txt") is None
