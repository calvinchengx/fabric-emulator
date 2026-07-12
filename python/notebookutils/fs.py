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
import urllib.parse

from ._config import config
from ._http import request
from . import credentials

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
    base = f"{cfg.fabric_url}/{filesystem}"
    return base + ("/" + subpath if subpath else "")


def _headers():
    return {"Host": config().onelake_host, "x-ms-version": "2021-06-08",
            "Authorization": "Bearer " + _storage_token()}


def put(path, content, overwrite=True):
    """Write `content` (str or bytes) to `path` via create → append → flush."""
    if isinstance(content, str):
        content = content.encode()
    fs, sub = _resolve(path)
    url = _url(fs, sub)
    request("PUT", url + "?resource=file", headers=_headers())
    if content:
        request("PATCH", url + "?action=append&position=0", body=content, headers=_headers())
    request("PATCH", f"{url}?action=flush&position={len(content)}", headers=_headers())


def append(path, content):
    """Append `content` at end-of-file (read length, append, flush)."""
    if isinstance(content, str):
        content = content.encode()
    fs, sub = _resolve(path)
    url = _url(fs, sub)
    status, hdrs, _ = request("HEAD", url, headers=_headers(), raw=True)
    pos = int(hdrs.get("Content-Length", "0"))
    request("PATCH", f"{url}?action=append&position={pos}", body=content, headers=_headers())
    request("PATCH", f"{url}?action=flush&position={pos + len(content)}", headers=_headers())


def head(path, maxBytes=None):
    """Return the first `maxBytes` (default whole file) of `path` as text."""
    fs, sub = _resolve(path)
    hdrs = _headers()
    if maxBytes is not None:
        hdrs["Range"] = f"bytes=0-{maxBytes - 1}"
    _, _, body = request("GET", _url(fs, sub), headers=hdrs, raw=True)
    return body.decode("utf-8", "replace")


def read(path):
    """Return the full bytes of `path`."""
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


def mkdirs(path):
    fs, sub = _resolve(path)
    request("PUT", _url(fs, sub) + "?resource=directory", headers=_headers())


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


def cp(src, dst):
    """Copy `src` to `dst` (read-through; sufficient for notebook-scale files)."""
    put(dst, read(src))


def rm(path, recursive=False):
    fs, sub = _resolve(path)
    url = _url(fs, sub)
    if recursive:
        url += "?recursive=true"
    request("DELETE", url, headers=_headers())
