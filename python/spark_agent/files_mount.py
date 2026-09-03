"""Mirror a lakehouse's Files/ at /lakehouse/default/Files, like Fabric's mount.

A real Fabric runtime FUSE-mounts the notebook's default lakehouse at
/lakehouse/default, and notebook code reads relative paths against it: contract
files, config JSON, reference data. The emulator had no equivalent, so anything
reading /lakehouse/default/Files failed with FileNotFoundError until someone
docker-cp'd the files into the container by hand, and that staging silently
vanished on every container recreate.

The control plane now posts /mount when it binds a session to a lakehouse, and
this module mirrors that lakehouse's Files/ tree to the local mount point via
the OneLake DFS API (the same route notebookutils.fs uses, agent's own token).
The Livy agent then calls refresh() at every statement boundary so the mount
stays a two-way, per-statement analog of the FUSE mount rather than a bind-time
snapshot.

DIVERGENCES FROM REAL FABRIC, stated rather than hidden:
  - Fresh at every statement, not live FUSE. A write to the mount lands in
    OneLake at statement end; a file uploaded to OneLake appears at the next
    statement, not mid-cell.
  - Deletes are not propagated. A local unlink is restored on the next pull.
  - One mount point. Sessions bound to DIFFERENT lakehouses share
    /lakehouse/default; a second bind of a different lakehouse is refused
    (docs/37 §2c) rather than silently replacing the first session's files.
"""
from __future__ import annotations

import os
import posixpath
import threading
from typing import TypedDict


class _MountState(TypedDict):
    workspace: str | None
    lakehouse: str | None
    seen: dict[str, tuple[int, float]]


MOUNT_ROOT = "/lakehouse/default/Files"
_state: _MountState = {"workspace": None, "lakehouse": None, "seen": {}}
# ThreadingHTTPServer serves /statements and /submit at once. refresh/flush/sync
# mutate this snapshot, and spark-submit reads the tree for up to 900s. Without
# a lock a torn `_state["seen"]` skips a flush or a submit reads a half-pulled
# tree. hold() is the same lock, held across a submit so a statement refresh
# waits rather than rewriting Files/ under spark-submit.
_lock = threading.RLock()


def hold():
    """Held while spark-submit reads the mount. Same lock as sync/refresh/flush."""
    return _lock


def _under_mount(rel: str):
    """MOUNT_ROOT/rel, or None if that would land outside MOUNT_ROOT.

    `rel` comes from a remote listing, so it is untrusted input: a lakehouse
    entry named `../../etc/whatever` would otherwise have os.path.join happily
    walk out of the mount and the writer overwrite a file on the container
    disk. Real Fabric's FUSE mount cannot be escaped this way; neither should
    this mirror of it.

    Resolved with realpath so a symlink already inside the mount cannot be used
    as the escape either, and compared with commonpath rather than a string
    prefix — `/lakehouse/default/Files-evil` starts with the root as a string
    while being a different directory.
    """
    root = os.path.realpath(MOUNT_ROOT)
    local = os.path.realpath(os.path.join(root, rel))
    try:
        if os.path.commonpath([root, local]) != root:
            return None
    except ValueError:  # pragma: no cover - different drives on Windows
        return None
    return local


def _import_fs():
    # The notebookutils package ships under /app/python, which reaches sys.path
    # when a SESSION initialises (agent._notebookutils). /mount arrives at
    # session BIND, before any statement, so on a fresh agent that path is not
    # there yet and a bare import fails — which made the first notebook run
    # after every agent recreate silently lose its mount while all later runs
    # worked. Ensure the path ourselves; never depend on call order.
    import sys
    pkg_root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    if pkg_root not in sys.path:
        sys.path.insert(0, pkg_root)
    try:
        from notebookutils import fs
        return fs
    except ImportError:  # pragma: no cover - image without the package
        return None


def _files_uri(workspace: str, lakehouse: str, rel: str = "") -> str:
    base = f"abfss://{workspace}@onelake.dfs.fabric.microsoft.com/{lakehouse}/Files"
    if not rel or rel in (".", os.curdir):
        return base
    return f"{base}/{rel.replace(os.sep, '/')}"


def _snapshot() -> dict:
    """relposix -> (size, mtime) for regular files currently under the mount."""
    seen = {}
    root = os.path.realpath(MOUNT_ROOT)
    if not os.path.isdir(root):
        return seen
    for dirpath, dirnames, filenames in os.walk(root, followlinks=False):
        # A symlink dir is an escape hatch; do not walk into it.
        dirnames[:] = [d for d in dirnames
                       if not os.path.islink(os.path.join(dirpath, d))]
        for name in filenames:
            local = os.path.join(dirpath, name)
            if os.path.islink(local) or not os.path.isfile(local):
                continue
            resolved = os.path.realpath(local)
            try:
                if os.path.commonpath([root, resolved]) != root:
                    continue
            except ValueError:  # pragma: no cover - different drives on Windows
                continue
            rel = os.path.relpath(resolved, root).replace(os.sep, "/")
            seen[rel] = (os.path.getsize(local), os.path.getmtime(local))
    return seen


def _conflict(requested: str) -> dict:
    current = _state["lakehouse"]
    return {
        "mounted": False,
        "current": current,
        "requested": requested,
        "error": (
            f"this agent already has lakehouse {current} mounted and cannot "
            f"isolate {requested} — Fabric gives each session its own "
            f"container and the emulator runs one process (docs/37). Bind "
            f"the same lakehouse, or run the sessions against separate agents."
        ),
    }


def _pull(fs, workspace: str, lakehouse: str) -> dict:
    """OneLake -> local. Size-matched files are kept; the rest are copied."""
    base = _files_uri(workspace, lakehouse)
    copied = kept = failed = 0

    def walk(prefix: str):
        nonlocal copied, kept, failed
        try:
            entries = fs.ls(prefix)
        except Exception:
            return  # no Files/ yet: the normal state of a fresh lakehouse
        for e in entries:
            rel = e.path.split("/Files/", 1)[-1] if "/Files/" in e.path else e.name
            local = _under_mount(rel)
            if local is None:
                failed += 1
                print(f"files_mount: refusing {e.path}: escapes the mount root",
                      flush=True)
                continue
            if e.isDir:
                os.makedirs(local, exist_ok=True)
                walk(posixpath.join(prefix, e.name))
                continue
            try:
                if os.path.isfile(local) and os.path.getsize(local) == e.size:
                    kept += 1
                    continue
                os.makedirs(os.path.dirname(local), exist_ok=True)
                data = fs.read(e.path)
                if isinstance(data, str):
                    data = data.encode()
                with open(local, "wb") as f:
                    f.write(data)
                copied += 1
            except Exception as exc:  # noqa: BLE001 - one bad file must not kill the mount
                failed += 1
                print(f"files_mount: {e.path}: {exc}", flush=True)

    os.makedirs(MOUNT_ROOT, exist_ok=True)
    walk(base)
    return {"copied": copied, "kept": kept, "failed": failed}


def flush() -> dict:
    """Local -> OneLake for files that changed since the last pull/flush.

    No-op when nothing is mounted. Failures are counted, not raised: a single
    bad put must not fail the statement that produced the file.
    """
    with _lock:
        workspace, lakehouse = _state["workspace"], _state["lakehouse"]
        if not workspace or not lakehouse:
            return {"flushed": 0, "flush_failed": 0}
        fs = _import_fs()
        if fs is None:
            return {"flushed": 0, "flush_failed": 0, "error": "notebookutils unavailable"}
        return _flush(fs, workspace, lakehouse)


def _flush(fs, workspace: str, lakehouse: str) -> dict:
    flushed = failed = 0
    current = _snapshot()
    for rel, stamp in current.items():
        if _state["seen"].get(rel) == stamp:
            continue
        local = _under_mount(rel)
        if local is None:
            failed += 1
            print(f"files_mount: refusing flush of {rel}: escapes the mount root",
                  flush=True)
            continue
        try:
            with open(local, "rb") as f:
                data = f.read()
            fs.put(_files_uri(workspace, lakehouse, rel), data, overwrite=True)
            flushed += 1
        except Exception as exc:  # noqa: BLE001 - one bad file must not kill the flush
            failed += 1
            print(f"files_mount: flush {rel}: {exc}", flush=True)
    _state["seen"] = _snapshot()
    if flushed or failed:
        print(f"files_mount: flush {lakehouse} "
              f"(flushed={flushed} failed={failed})", flush=True)
    return {"flushed": flushed, "flush_failed": failed}


def refresh() -> dict:
    """Statement boundary: write local changes back, then pull OneLake.

    This is "fresh at every statement", not live FUSE. No-op until /mount has
    bound a lakehouse.
    """
    with _lock:
        workspace, lakehouse = _state["workspace"], _state["lakehouse"]
        if not workspace or not lakehouse:
            return {"refreshed": False}
        fs = _import_fs()
        if fs is None:
            return {"refreshed": False, "error": "notebookutils unavailable"}
        out = _flush(fs, workspace, lakehouse)
        pulled = _pull(fs, workspace, lakehouse)
        _state["seen"] = _snapshot()
        return {"refreshed": True, "lakehouse": lakehouse, **out, **pulled}


def sync(workspace: str, lakehouse: str) -> dict:
    """Mirror abfss://{workspace}/{lakehouse}/Files into MOUNT_ROOT.

    Returns a summary dict for the control plane's log. Failures are reported,
    not raised: a missing Files/ area is the normal state of a fresh lakehouse
    and must not fail session creation.

    A second bind of a *different* lakehouse is refused and leaves the first
    mount untouched. Re-binding the same lakehouse flushes then re-pulls.
    """
    with _lock:
        fs = _import_fs()
        if fs is None:
            return {"mounted": False, "error": "notebookutils unavailable"}

        if _state["lakehouse"] not in (None, lakehouse):
            refused = _conflict(lakehouse)
            print(f"files_mount: {refused['error']}", flush=True)
            return refused

        if _state["lakehouse"] == lakehouse:
            # Re-bind of the same lakehouse (every notebook run): flush first so
            # in-flight local writes are not clobbered by the pull.
            flushed = _flush(fs, workspace, lakehouse)
        else:
            flushed = {"flushed": 0, "flush_failed": 0}

        _state["workspace"] = workspace
        _state["lakehouse"] = lakehouse
        pulled = _pull(fs, workspace, lakehouse)
        _state["seen"] = _snapshot()
        summary = {"mounted": True, "lakehouse": lakehouse, **pulled, **flushed}
        print(f"files_mount: {lakehouse} -> {MOUNT_ROOT} "
              f"(copied={pulled['copied']} kept={pulled['kept']} "
              f"failed={pulled['failed']})", flush=True)
        return summary
