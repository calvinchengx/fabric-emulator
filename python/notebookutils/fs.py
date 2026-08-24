"""notebookutils.fs — file operations against OneLake.

Speaks the ADLS Gen2 DFS surface the same way Spark's ABFS driver does:
create → append → flush for writes, ranged GET for reads, `resource=filesystem`
for listings. Accepts `abfss://ws@onelake.dfs.fabric.microsoft.com/item/path`
URIs (what real notebooks pass) and lakehouse-relative paths ("Files/…",
"Tables/…") resolved against the default lakehouse in the runtime context.

The DFS surface is host-routed, so every request carries the OneLake Host
header while connecting to the emulator's address — the emulator routes on Host,
not DNS, so no /etc/hosts trickery is needed from Python.
"""
import errno
import os
import urllib.parse

from . import credentials
from ._config import config
from ._http import HttpError, request

# OneLake authorizes with a Storage-audience token, minted once for the
# notebook identity and reused across fs calls (as the real driver does).
_token = None


def _storage_token():
    global _token
    if _token is None:
        _token = credentials.getToken("storage")
    return _token


class FileInfo:
    def __init__(self, path, name, size, is_dir):
        self.path = path
        self.name = name
        self.size = size
        self.isDir = is_dir
        self.isFile = not is_dir

    def __repr__(self):
        kind = "dir" if self.isDir else "file"
        return f"FileInfo({kind} {self.path!r} size={self.size})"


def _resolve(path):
    """Return (filesystem, subpath) — the workspace container and the
    item-relative path within it — for an abfss URI or a relative path."""
    cfg = config()
    parsed = urllib.parse.urlparse(path)
    if parsed.scheme in ("abfss", "abfs"):
        filesystem = parsed.netloc.split("@", 1)[0]
        return filesystem, parsed.path.lstrip("/")
    # Relative path → default workspace + lakehouse from the runtime context.
    if not cfg.workspace_id or not cfg.lakehouse_id:
        raise RuntimeError(
            f"relative path {path!r} needs a default lakehouse; set "
            "NOTEBOOKUTILS_WORKSPACE_ID and NOTEBOOKUTILS_LAKEHOUSE_ID, or pass an abfss:// URI"
        )
    return cfg.workspace_id, f"{cfg.lakehouse_id}/{path.lstrip('/')}"


def _url(filesystem, subpath=""):
    cfg = config()
    base = f"{cfg.onelake_url}/{filesystem}"
    return base + ("/" + subpath if subpath else "")


def _cell_context():
    """Identify the notebook cell making this request, when the runtime says so.

    A notebook runtime executes cells one at a time, so it always knows which
    one is running; it exports that as FABRIC_JOB_ID / FABRIC_CELL_INDEX before
    each cell. Passing it here lets the emulator attribute the I/O it actually
    serves to the cell that caused it — observed lineage, with no parsing of
    user code. Absent outside a notebook run, in which case nothing is tagged.
    """
    job = os.environ.get("FABRIC_JOB_ID", "")
    cell = os.environ.get("FABRIC_CELL_INDEX", "")
    if not job or not cell:
        return {}
    return {"x-ms-fabric-job-id": job, "x-ms-fabric-cell-index": cell}


def _headers():
    return {"Host": config().onelake_host, "x-ms-version": "2021-06-08",
            "Authorization": "Bearer " + _storage_token(),
            **_cell_context()}


# THE DOCUMENTED SPELLING, not the reasonable one. Phase 0 (docs/56) found five
# members here that existed, worked, and would still be declined by a framework
# introspecting them: `path` for `file`, `dst` for `dest`, `maxBytes` for
# `max_bytes`, `recursive` for `recurse`. Each was the spelling somebody picks
# writing a method from its description rather than from the page. Contract 2's
# asymmetry is about NAMES, not counts -- a caller that passes `file=` gets a
# TypeError from the reasonable spelling, without ever reaching the body.


def put(file, content, overwrite=False):
    """Write `content` (str or bytes) to `file` via create → append → flush."""
    if isinstance(content, str):
        content = content.encode()
    fs, sub = _resolve(file)
    url = _url(fs, sub)
    request("PUT", url + "?resource=file", headers=_headers())
    if content:
        request("PATCH", url + "?action=append&position=0", body=content, headers=_headers())
    request("PATCH", f"{url}?action=flush&position={len(content)}", headers=_headers())


def append(file, content, createFileIfNotExists=False):  # noqa: N803 - documented spelling
    """Append `content` at end-of-file (read length, append, flush)."""
    if isinstance(content, str):
        content = content.encode()
    fs, sub = _resolve(file)
    url = _url(fs, sub)
    try:
        _status, hdrs, _ = request("HEAD", url, headers=_headers(), raw=True)
        pos = int(hdrs.get("Content-Length", "0"))
    except Exception as exc:
        if getattr(exc, "status", None) != 404 or not createFileIfNotExists:
            raise
        # The flag is the whole difference between "append to a file" and
        # "start one", and Fabric makes the caller say which they meant.
        request("PUT", url + "?resource=file", headers=_headers())
        pos = 0
    request("PATCH", f"{url}?action=append&position={pos}", body=content, headers=_headers())
    request("PATCH", f"{url}?action=flush&position={pos + len(content)}", headers=_headers())


# 1024 * 100, exactly as the page writes it. `head` is a PREVIEW, and the
# default is part of that meaning: a caller wanting every byte wants `read`.
# The shim used to default to the whole file, and `json_multiline` depended on
# it -- a dependency on a divergence, corrected with this.
HEAD_DEFAULT_MAX_BYTES = 1024 * 100


def head(file, max_bytes=HEAD_DEFAULT_MAX_BYTES):
    """Return up to the first `max_bytes` of `file` as text."""
    fs, sub = _resolve(file)
    hdrs = _headers()
    if max_bytes is not None:
        hdrs["Range"] = f"bytes=0-{max_bytes - 1}"
    _, _, body = request("GET", _url(fs, sub), headers=hdrs, raw=True)
    return body.decode("utf-8", "replace")


def read(path):
    """Return the full bytes of `path`.

    NOT part of the documented surface -- contract 2 allows extra members, and
    this one is what `head`'s truncation makes necessary for a whole-file read.
    """
    fs, sub = _resolve(path)
    _, _, body = request("GET", _url(fs, sub), headers=_headers(), raw=True)
    return body


def exists(path):
    fs, sub = _resolve(path)
    try:
        request("HEAD", _url(fs, sub), headers=_headers(), raw=True)
        return True
    except Exception as e:
        if getattr(e, "status", None) == 404:
            return False
        raise


def getProperties(path):  # noqa: N802 - documented spelling
    """Path metadata as a name→value map, from the DFS response headers."""
    fs, sub = _resolve(path)
    _status, hdrs, _ = request("HEAD", _url(fs, sub), headers=_headers(), raw=True)
    return dict(hdrs)


def mkdirs(path):
    fs, sub = _resolve(path)
    request("PUT", _url(fs, sub) + "?resource=directory", headers=_headers())
    return True


def ls(path):
    """List the directory `path`, returning FileInfo entries."""
    fs, sub = _resolve(path)
    q = urllib.parse.urlencode({"resource": "filesystem", "recursive": "false", "directory": sub})
    resp = request("GET", f"{_url(fs)}?{q}", headers=_headers())
    out = []
    for p in resp.get("paths", []):
        name = p.get("name", "")
        is_dir = str(p.get("isDirectory", "false")).lower() == "true"
        out.append(FileInfo(f"abfss://{fs}@{config().onelake_host}/{name}",
                            name.rsplit("/", 1)[-1], int(p.get("contentLength", 0) or 0), is_dir))
    return out


def _copy_one(src, dest):
    put(dest, read(src))


def _copy_tree(src, dest):
    """Copy a directory. Recursion is the caller's opt-in, per `recurse`."""
    mkdirs(dest)
    for entry in ls(src):
        child_dest = dest.rstrip("/") + "/" + entry.name
        if entry.isDir:
            _copy_tree(entry.path, child_dest)
        else:
            _copy_one(entry.path, child_dest)


def cp(src, dest, recurse=False):
    """Copy `src` to `dest`. Set `recurse` for directories."""
    if recurse:
        _copy_tree(src, dest)
    else:
        _copy_one(src, dest)
    return True


def fastcp(src, dest, recurse=True, extraConfigs=None):  # noqa: N803 - documented spelling
    """`cp` by another name here, and the difference is stated rather than faked.

    On Fabric this routes through azcopy for throughput on large transfers, and
    `recurse` defaults to True where `cp`'s defaults to False. The emulator has
    no azcopy and nothing to gain from one at notebook scale, so the COPY is the
    same code — but the defaults are Fabric's, because a caller relying on
    `fastcp`'s recursive-by-default and getting `cp`'s would silently copy one
    file instead of a tree.
    """
    return cp(src, dest, recurse=recurse)


def mv(src, dest, create_path=True, overwrite=False):
    """Move `src` to `dest` (copy, then remove the source).

    `create_path` defaults to True: the page records that Python notebooks
    default it True and Spark notebooks False, and this shim is the Python
    surface. Stated because the split is a real divergence a caller can hit.
    """
    if not overwrite and exists(dest):
        raise FileExistsError(
            f"{dest} exists and overwrite=False — pass overwrite=True to replace it")
    if create_path:
        parent = dest.rstrip("/").rsplit("/", 1)[0]
        if parent and parent != dest.rstrip("/"):
            mkdirs(parent)
    if _is_dir(src):
        cp(src, dest, recurse=True)
    else:
        _copy_one(src, dest)
    rm(src, recurse=True)
    return True


def _is_dir(path):
    try:
        return str(getProperties(path).get("x-ms-resource-type", "")).lower() == "directory"
    except Exception:  # noqa: BLE001 - unreadable is not evidence of a directory
        return False


def rm(path, recurse=False):
    """Remove a file, or a directory and optionally its contents.

    `recurse` is IGNORED for a file and REQUIRED for a non-empty directory —
    ADLS answers 409 DirectoryNotEmpty otherwise, which is the whole safety of
    a bare `rm`. Translated to OSError/ENOTEMPTY rather than surfaced as an
    HTTP error, for the same reason `mv` raises FileExistsError: a notebook
    author catches filesystem exceptions, and the mapping here is exact — this
    is what `os.rmdir` raises for the identical situation.
    """
    fs, sub = _resolve(path)
    url = _url(fs, sub)
    if recurse:
        url += "?recursive=true"
    try:
        request("DELETE", url, headers=_headers())
    except HttpError as e:
        if e.status == 409:
            raise OSError(errno.ENOTEMPTY,
                          f"{path} is a non-empty directory — pass recurse=True "
                          f"to remove it and its contents") from None
        raise
    return True


# ---------------------------------------------------------------------------
# Mount and unmount.
#
# WHAT A MOUNT IS FOR, and therefore what has to be emulated: a LOCAL FILE PATH
# that reaches remote data, so code expecting a filesystem — `open()`, a library
# that takes a path, anything not written for object storage — works unchanged.
# That observable contract is materialisable here: copy the remote subtree to a
# local directory and hand back its path.
#
# WHAT IS NOT EMULATED, said plainly rather than discovered: Fabric's mount is
# backed by blobfuse and is LIVE — it fetches on access, with a cache timeout.
# This one is a point-in-time copy taken at mount. A file changed remotely after
# the mount is not seen, and a file written locally does not travel back.
# `fileCacheTimeout` and `timeout` in `extraConfigs` are accepted and ignored,
# because there is no cache here for them to tune; accepting them is correct
# emulation (there is nothing to switch), inventing behaviour for them would not
# be. Recorded in docs/56 rather than left for someone to find at 2am.

_MOUNTS = {}


class MountPointInfo:
    """One entry of `mounts()`. Attribute names are the ones Fabric's examples
    read — `m.mountPoint` — so a loop over `mounts()` ports unchanged."""

    def __init__(self, source, mountPoint, localPath):  # noqa: N803 - documented spelling
        self.source = source
        self.mountPoint = mountPoint
        self.localPath = localPath

    def __repr__(self):
        return f"MountPointInfo({self.mountPoint!r} -> {self.source!r})"


def _mount_root(mountPoint):  # noqa: N803 - documented spelling
    """Fabric's shape: /synfs/notebook/{sessionId}{mountPoint}.

    The session id comes from the runtime context so two sessions in one agent
    do not share a mount directory — the same isolation contract 5 asserts for
    catalogs, applied to the filesystem.
    """
    import tempfile

    session = os.environ.get("FABRIC_JOB_ID") or "local"
    base = os.path.join(tempfile.gettempdir(), "synfs", "notebook", session)
    return os.path.join(base, mountPoint.lstrip("/"))


def mount(source, mountPoint, extraConfigs=None):  # noqa: N803 - documented spelling
    """Materialise `source` under a local path and remember the mapping."""
    if mountPoint in _MOUNTS:
        # Fabric's own guidance is to check `mounts()` first; re-mounting the
        # same point is the caller's error, not something to silently redo.
        raise ValueError(
            f"{mountPoint} is already mounted from {_MOUNTS[mountPoint].source} — "
            "unmount it first, or check notebookutils.fs.mounts() before mounting")
    local = _mount_root(mountPoint)
    os.makedirs(local, exist_ok=True)
    _materialise(source, local)
    _MOUNTS[mountPoint] = MountPointInfo(source, mountPoint, local)
    return True


def _materialise(source, local):
    """Copy the remote subtree to `local`. A single file mounts as itself."""
    try:
        entries = ls(source)
    except Exception:  # noqa: BLE001 - not a directory; treat as a single file
        entries = None
    if entries is None:
        with open(os.path.join(local, source.rstrip("/").rsplit("/", 1)[-1]), "wb") as fh:
            fh.write(read(source))
        return
    for entry in entries:
        target = os.path.join(local, entry.name)
        if entry.isDir:
            os.makedirs(target, exist_ok=True)
            _materialise(entry.path, target)
        else:
            with open(target, "wb") as fh:
                fh.write(read(entry.path))


def unmount(mountPoint, extraConfigs=None):  # noqa: N803 - documented spelling
    """Remove a mount point and its local copy."""
    info = _MOUNTS.pop(mountPoint, None)
    if info is None:
        return False
    import shutil

    shutil.rmtree(info.localPath, ignore_errors=True)
    return True


def mounts(extraOptions=None):  # noqa: N803 - documented spelling
    """Every current mount point."""
    return list(_MOUNTS.values())


def getMountPath(mountPoint, scope=""):  # noqa: N802,N803 - documented spelling
    """The local filesystem path for `mountPoint`."""
    info = _MOUNTS.get(mountPoint)
    if info is None:
        raise KeyError(
            f"{mountPoint} is not mounted — mount it first, or check "
            "notebookutils.fs.mounts()")
    return info.localPath
