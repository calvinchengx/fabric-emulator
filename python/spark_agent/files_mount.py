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

DIVERGENCES FROM REAL FABRIC, stated rather than hidden:
  - One-way, read only. A notebook WRITE to /lakehouse/default/Files lands on
    the container disk and does not propagate back to OneLake; on Fabric it
    would. Notebook code in this codebase writes through abfss paths, which is
    why the read direction is the one that was blocking.
  - Sync-at-bind, not live. Files uploaded to OneLake after the session bound
    appear at the next bind, not immediately.
  - One mount point, last bind wins. Sessions bound to DIFFERENT lakehouses
    share /lakehouse/default; concurrent runs with different bindings would
    fight over it, so a bind that switches the mounted lakehouse logs loudly.

Files are skipped when the local copy already matches by size, so re-binding a
session (every notebook run) costs one listing plus only the changed bytes.
"""
from __future__ import annotations

import os
import posixpath

MOUNT_ROOT = "/lakehouse/default/Files"
_state = {"lakehouse": None}



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
    except ValueError:  # different drives on Windows
        return None
    return local

def sync(workspace: str, lakehouse: str) -> dict:
    """Mirror abfss://{workspace}/{lakehouse}/Files into MOUNT_ROOT.

    Returns a summary dict for the control plane's log. Failures are reported,
    not raised: a missing Files/ area is the normal state of a fresh lakehouse
    and must not fail session creation.
    """
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
    except ImportError:  # pragma: no cover - image without the package
        return {"mounted": False, "error": "notebookutils unavailable"}

    if _state["lakehouse"] not in (None, lakehouse):
        print(f"files_mount: /lakehouse/default switching from lakehouse "
              f"{_state['lakehouse']} to {lakehouse}; concurrent sessions bound "
              f"to different lakehouses share this one mount point", flush=True)
    _state["lakehouse"] = lakehouse

    base = f"abfss://{workspace}@onelake.dfs.fabric.microsoft.com/{lakehouse}/Files"
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
    summary = {"mounted": True, "lakehouse": lakehouse,
               "copied": copied, "kept": kept, "failed": failed}
    print(f"files_mount: {lakehouse} -> {MOUNT_ROOT} "
          f"(copied={copied} kept={kept} failed={failed})", flush=True)
    return summary
