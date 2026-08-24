"""The shim surface a notebook actually calls, driven with the HTTP layer stubbed.

`fs`, `lakehouse` and `credentials` were covered only by `e2e/notebookutils` —
a suite that boots three binaries. Real coverage, but it runs late, it cannot
easily reach an error path, and until now nothing exercised the URL and header
construction that decides whether a request goes to the right place at all.

Everything funnels through one chokepoint (`_http.request`), which is what makes
this cheap: stub it and the whole surface becomes assertable. The same technique
took `notebook.py` to 99% in #55.

WHAT IS UNDER TEST is the shim's own logic — path resolution, URL shape, the
Host header the emulator routes on, ranged reads, the create/append/flush write
dance. Not whether OneLake answers; that is the e2e's job and it keeps it.
"""
import errno
import json
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

from notebookutils import credentials, env, fs, lakehouse, runtime, session, udf  # noqa: E402
from notebookutils._http import HttpError  # noqa: E402

WS = "11111111-1111-1111-1111-111111111111"
LH = "22222222-2222-2222-2222-222222222222"


class Recorder:
    """Stands in for `_http.request`, recording calls and replaying answers."""

    def __init__(self):
        self.calls = []
        self.answers = []

    def push(self, value):
        self.answers.append(value)
        return self

    def __call__(self, method, url, *, token=None, body=None, headers=None, raw=False):
        self.calls.append({"method": method, "url": url, "token": token,
                           "body": body, "headers": headers or {}, "raw": raw})
        if self.answers:
            answer = self.answers.pop(0)
            if isinstance(answer, Exception):
                raise answer
            return answer
        return {}

    def urls(self):
        return [c["url"] for c in self.calls]

    def last(self):
        return self.calls[-1]


@pytest.fixture
def http(monkeypatch):
    """Stub the one HTTP chokepoint in every shim module that uses it."""
    rec = Recorder()
    for mod in (fs, lakehouse, credentials, session, udf):
        monkeypatch.setattr(mod, "request", rec, raising=False)
    monkeypatch.setattr(credentials, "getToken", lambda audience: f"tok-{audience}")
    monkeypatch.setattr(fs, "_token", None)  # the module caches one
    cfg = type("C", (), {
        "fabric_url": "https://localhost:9443",
        "onelake_url": "https://localhost:9443",
        "onelake_host": "onelake.dfs.fabric.microsoft.com",
        "workspace_id": WS,
        "lakehouse_id": LH,
        "vault_url": "https://localhost:8444",
        "is_real": False,
    })()
    for mod in (fs, lakehouse, credentials, runtime, udf):
        monkeypatch.setattr(mod, "config", lambda: cfg, raising=False)
    return rec


# --- path resolution ----------------------------------------------------------

def test_a_relative_path_resolves_against_the_default_lakehouse(http):
    fs.put("Files/greeting.txt", "hi")
    assert f"/{WS}/{LH}/Files/greeting.txt" in http.urls()[0]


def test_an_abfss_uri_is_used_verbatim(http):
    # A notebook written for real Fabric passes these; the workspace comes from
    # the URI, not the runtime context.
    other = "33333333-3333-3333-3333-333333333333"
    fs.put(f"abfss://{other}@onelake.dfs.fabric.microsoft.com/item/Files/a.txt", "x")
    url = http.urls()[0]
    assert f"/{other}/item/Files/a.txt" in url
    assert WS not in url


def test_a_relative_path_without_a_default_lakehouse_says_which_knob(http, monkeypatch):
    bare = type("C", (), {"workspace_id": "", "lakehouse_id": "",
                          "onelake_url": "x", "onelake_host": "h"})()
    monkeypatch.setattr(fs, "config", lambda: bare)
    with pytest.raises(RuntimeError, match="NOTEBOOKUTILS_LAKEHOUSE_ID"):
        fs.put("Files/a.txt", "x")


def test_every_request_carries_the_onelake_host_header(http):
    # The emulator routes the DFS surface on Host, not DNS. Without this header
    # the request reaches the control plane instead, which is a 404 that looks
    # like a missing file.
    fs.put("Files/a.txt", "x")
    assert http.last()["headers"].get("Host") == "onelake.dfs.fabric.microsoft.com"


# --- writes: the create → append → flush dance --------------------------------

def test_put_creates_appends_and_flushes(http):
    fs.put("Files/a.txt", "hello")
    methods = [(c["method"], c["url"].split("?", 1)[-1]) for c in http.calls]
    assert methods[0][0] == "PUT" and "resource=file" in methods[0][1]
    assert methods[1][0] == "PATCH" and "action=append" in methods[1][1]
    assert methods[2][0] == "PATCH" and "action=flush" in methods[2][1]


def test_put_sends_the_content_as_bytes(http):
    fs.put("Files/a.txt", "hello")
    append = next(c for c in http.calls if c["method"] == "PATCH" and "append" in c["url"])
    assert append["body"] == b"hello"


def test_append_reads_the_current_length_then_appends_at_it(http):
    # Appending at offset 0 would overwrite. The shim must learn the existing
    # size first, which is what makes this more than a second put.
    http.push((200, {"Content-Length": "5"}, b"")).push({}).push({})
    fs.append("Files/a.txt", " more")
    appended = next(c for c in http.calls if "action=append" in c["url"])
    assert "position=5" in appended["url"]


# --- reads --------------------------------------------------------------------

def test_head_asks_for_a_bounded_range(http):
    # `head` must not pull a multi-GB file to show its first bytes.
    http.push((200, {}, b"hello"))
    assert fs.head("Files/a.txt", 5) == "hello"
    assert http.last()["headers"].get("Range") == "bytes=0-4"


def test_read_returns_raw_bytes(http):
    http.push((200, {}, b"\x00\x01binary"))
    assert fs.read("Files/a.bin") == b"\x00\x01binary"


def test_exists_is_true_on_a_successful_head(http):
    http.push((200, {}, b""))
    assert fs.exists("Files/a.txt") is True


def test_exists_is_false_on_a_404_rather_than_raising(http):
    from notebookutils._http import HttpError
    http.push(HttpError(404, "not found", "u"))
    assert fs.exists("Files/nope.txt") is False


def test_a_non_404_error_still_propagates_from_exists(http):
    # A 403 means the caller's token is wrong, not that the file is absent.
    # Swallowing it into False is how a permissions bug reads as a missing file.
    from notebookutils._http import HttpError
    http.push(HttpError(403, "forbidden", "u"))
    with pytest.raises(HttpError):
        fs.exists("Files/a.txt")


# --- listing ------------------------------------------------------------------

def test_ls_returns_fileinfo_with_sizes_and_directory_flags(http):
    http.push({"paths": [
        {"name": f"{LH}/Files/a.txt", "contentLength": "12"},
        {"name": f"{LH}/Files/sub", "isDirectory": "true"},
    ]})
    got = fs.ls("Files")
    assert [f.name for f in got] == ["a.txt", "sub"]
    assert got[0].isFile and got[0].size == 12
    assert got[1].isDir and got[1].isFile is False


def test_ls_uses_the_filesystem_resource_surface(http):
    http.push({"paths": []})
    fs.ls("Files")
    assert "resource=filesystem" in http.last()["url"]


def test_fileinfo_repr_says_what_it_is():
    info = fs.FileInfo("abfss://x/y", "y", 12, False)
    assert "file" in repr(info) and "12" in repr(info)


# --- lakehouse control plane --------------------------------------------------

def test_lakehouse_list_hits_the_workspace_scoped_endpoint(http):
    http.push({"value": [{"displayName": "lake", "id": LH}]})
    assert [x["displayName"] for x in lakehouse.list()] == ["lake"]
    assert http.last()["url"].endswith(f"/workspaces/{WS}/lakehouses")


def test_lakehouse_create_posts_the_display_name(http):
    lakehouse.create("bronze")
    assert http.last()["body"] == {"displayName": "bronze"}


def test_lakehouse_get_addresses_the_item_by_id(http):
    http.push({"id": LH})
    lakehouse.get(LH)
    assert http.last()["url"].endswith(f"/lakehouses/{LH}")


# --- credentials --------------------------------------------------------------

def test_get_secret_uses_the_pinned_vault_when_configured(http, monkeypatch):
    # The emulator serves its default vault on a non-DNS host, so a bare vault
    # NAME must still resolve locally.
    monkeypatch.undo()
    rec = Recorder().push({"value": "s3cr3t"})
    monkeypatch.setattr(credentials, "request", rec)
    monkeypatch.setattr(credentials, "getToken", lambda a: "tok")
    monkeypatch.setattr(credentials, "config", lambda: type("C", (), {
        "vault_url": "https://localhost:8444", "is_real": False})())
    assert credentials.getSecret("db-vault", "db-password") == "s3cr3t"
    assert rec.last()["url"].startswith("https://localhost:8444")


def test_get_secret_with_ls_is_an_alias(http, monkeypatch):
    monkeypatch.undo()
    rec = Recorder().push({"value": "v"}).push({"value": "v"})
    monkeypatch.setattr(credentials, "request", rec)
    monkeypatch.setattr(credentials, "getToken", lambda a: "tok")
    monkeypatch.setattr(credentials, "config", lambda: type("C", (), {
        "vault_url": "https://localhost:8444", "is_real": False})())
    assert credentials.getSecretWithLS("v", "n") == credentials.getSecret("v", "n")


# --- runtime context ----------------------------------------------------------

def test_runtime_context_supports_both_access_styles():
    ctx = runtime._Context({"currentWorkspaceId": WS, "defaultLakehouseId": LH})
    assert ctx["currentWorkspaceId"] == ctx.currentWorkspaceId == WS
    assert ctx.defaultLakehouseId == LH


def test_runtime_context_raises_attributeerror_for_an_unknown_key():
    # A KeyError escaping __getattr__ would break `hasattr` and `getattr(…, d)`,
    # which is how a framework probes for optional context fields.
    ctx = runtime._Context({"a": 1})
    with pytest.raises(AttributeError):
        _ = ctx.nope
    assert getattr(ctx, "nope", "fallback") == "fallback"


def test_the_real_runtime_context_carries_the_documented_keys():
    assert set(runtime.context) >= {"currentWorkspaceId", "defaultLakehouseId", "isForPipeline"}


def test_runtime_context_bind_is_the_running_notebook_not_the_process():
    # docs/38 §1: a framework that reads runtime.context (or env.getWorkspaceId)
    # must see the notebook that is running, not NOTEBOOKUTILS_* set out of band.
    token = runtime.bind({
        "currentWorkspaceId": WS,
        "defaultLakehouseId": LH,
        "currentNotebookId": "nb-1",
        "isForPipeline": True,
    })
    try:
        assert runtime.context.currentWorkspaceId == WS
        assert runtime.context["defaultLakehouseId"] == LH
        assert runtime.context["isForPipeline"] is True
        assert env.getWorkspaceId() == WS
        assert env.getLakehouseId() == LH
    finally:
        runtime.unbind(token)
    assert runtime.context.get("currentWorkspaceId") != WS


def test_runtime_context_bind_isolates_concurrent_statements():
    import concurrent.futures
    import time

    seen = {}

    def other():
        token = runtime.bind({"currentWorkspaceId": "other-ws"})
        try:
            time.sleep(0.05)
            seen["other"] = runtime.context["currentWorkspaceId"]
        finally:
            runtime.unbind(token)

    token = runtime.bind({"currentWorkspaceId": WS})
    try:
        with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
            fut = pool.submit(other)
            time.sleep(0.01)
            seen["this"] = runtime.context["currentWorkspaceId"]
            fut.result()
    finally:
        runtime.unbind(token)

    assert seen == {"this": WS, "other": "other-ws"}


# =============================================================================
# Phase 2 (docs/56): the documented surface, member by member.
#
# Every test below covers something a framework introspects or a notebook
# calls, and that did not exist — or existed under a name Fabric does not use —
# before the surface was cited.
# =============================================================================

# --- fs: the corrected spellings ---------------------------------------------


def test_fs_members_take_the_documented_parameter_names(http):
    """THE DEFECT PHASE 0 FOUND. `put(file=...)` is what a caller writes from
    the page; the shim used to take `path=` and raise TypeError before reaching
    the body — a framework introspecting it declines without ever calling."""
    fs.put(file="Files/a.txt", content="x")
    http.push((200, {}, b"hello"))
    fs.head(file="Files/a.txt", max_bytes=5)
    fs.rm(path="Files/a.txt", recurse=True)
    http.push((200, {}, b"hello"))
    fs.cp(src="Files/a.txt", dest="Files/b.txt", recurse=False)
    assert http.calls, "the documented spellings must reach the body"


def test_head_truncates_at_the_documented_default(http):
    """`head` is a PREVIEW. The default is 1024 * 100 exactly as the page
    writes it, and it is part of the meaning: a caller wanting every byte
    wants `read`."""
    http.push((200, {}, b"x" * 10))
    fs.head("Files/a.txt")
    assert fs.HEAD_DEFAULT_MAX_BYTES == 1024 * 100
    assert http.last()["headers"]["Range"] == f"bytes=0-{1024 * 100 - 1}"


def test_head_with_an_explicit_none_reads_the_whole_file(http):
    http.push((200, {}, b"hello"))
    assert fs.head("Files/a.txt", None) == "hello"
    assert "Range" not in http.last()["headers"]


# --- fs: append and its create flag ------------------------------------------


def test_append_to_a_missing_file_refuses_unless_told_to_create(http):
    """The flag is the whole difference between "append to a file" and "start
    one", and Fabric makes the caller say which they meant."""
    missing = Exception("no such file")
    missing.status = 404
    http.push(missing)
    with pytest.raises(Exception, match="no such file"):
        fs.append("Files/new.txt", "x")


def test_append_creates_the_file_when_asked(http):
    missing = Exception("no such file")
    missing.status = 404
    http.push(missing)
    fs.append("Files/new.txt", "x", createFileIfNotExists=True)
    urls = http.urls()
    assert any("resource=file" in u for u in urls), "it must create the file"
    assert any("action=append&position=0" in u for u in urls), "then append at 0"


def test_append_does_not_swallow_a_non_404(http):
    """A permission failure is not a missing file, and creating one in response
    would turn a 403 into a silent empty write."""
    denied = Exception("forbidden")
    denied.status = 403
    http.push(denied)
    with pytest.raises(Exception, match="forbidden"):
        fs.append("Files/a.txt", "x", createFileIfNotExists=True)


# --- fs: recursion, move, properties -----------------------------------------


def _entry(path, name, is_dir):
    return fs.FileInfo(path, name, 0, is_dir)


def test_cp_without_recurse_copies_one_file(http, monkeypatch):
    seen = []
    monkeypatch.setattr(fs, "read", lambda p: b"data")
    monkeypatch.setattr(fs, "put", lambda f, c, overwrite=False: seen.append(f))
    fs.cp("Files/a.txt", "Files/b.txt")
    assert seen == ["Files/b.txt"]


def test_cp_with_recurse_walks_the_tree(http, monkeypatch):
    """`recurse` is the caller's opt-in on `cp`, and defaults the other way on
    `fastcp` — so a tree copy that silently copied one file would be the
    difference between the two going unnoticed."""
    tree = {
        "Files/src": [_entry("Files/src/sub", "sub", True),
                      _entry("Files/src/a.txt", "a.txt", False)],
        "Files/src/sub": [_entry("Files/src/sub/b.txt", "b.txt", False)],
    }
    monkeypatch.setattr(fs, "ls", lambda p: tree.get(p, []))
    monkeypatch.setattr(fs, "read", lambda p: b"data")
    made, put = [], []
    monkeypatch.setattr(fs, "mkdirs", lambda p: made.append(p))
    monkeypatch.setattr(fs, "put", lambda f, c, overwrite=False: put.append(f))
    fs.cp("Files/src", "Files/dst", recurse=True)
    assert put == ["Files/dst/sub/b.txt", "Files/dst/a.txt"]
    assert "Files/dst" in made and "Files/dst/sub" in made


def test_fastcp_keeps_fabrics_recursive_default(http, monkeypatch):
    """The copy is the same code — there is no azcopy here and nothing to gain
    from one at notebook scale — but the DEFAULTS are Fabric's, because a
    caller relying on fastcp's recursive-by-default and getting cp's would
    silently copy one file instead of a tree."""
    calls = []
    monkeypatch.setattr(fs, "cp", lambda src, dest, recurse=False: calls.append(recurse))
    fs.fastcp("Files/src", "Files/dst")
    assert calls == [True]


def test_mv_refuses_to_clobber_unless_overwrite(http, monkeypatch):
    monkeypatch.setattr(fs, "exists", lambda p: True)
    with pytest.raises(FileExistsError, match="overwrite=True"):
        fs.mv("Files/a.txt", "Files/b.txt")


def test_mv_copies_then_removes_the_source(http, monkeypatch):
    monkeypatch.setattr(fs, "exists", lambda p: False)
    monkeypatch.setattr(fs, "_is_dir", lambda p: False)
    monkeypatch.setattr(fs, "read", lambda p: b"data")
    order = []
    monkeypatch.setattr(fs, "put", lambda f, c, overwrite=False: order.append(("put", f)))
    monkeypatch.setattr(fs, "mkdirs", lambda p: order.append(("mkdirs", p)))
    monkeypatch.setattr(fs, "rm", lambda p, recurse=False: order.append(("rm", p)))
    fs.mv("Files/a.txt", "Files/sub/b.txt")
    assert ("mkdirs", "Files/sub") in order
    assert order.index(("put", "Files/sub/b.txt")) < order.index(("rm", "Files/a.txt"))


def test_getProperties_returns_the_response_headers(http):
    http.push((200, {"Content-Length": "12", "x-ms-resource-type": "file"}, b""))
    assert fs.getProperties("Files/a.txt")["x-ms-resource-type"] == "file"


# --- fs: mount ----------------------------------------------------------------


@pytest.fixture
def clean_mounts():
    fs._MOUNTS.clear()
    yield
    fs._MOUNTS.clear()


def test_mount_materialises_the_source_and_hands_back_a_local_path(
        http, monkeypatch, clean_mounts, tmp_path):
    """WHAT A MOUNT IS FOR: a local path that reaches remote data, so code
    expecting a filesystem works unchanged. That observable contract is what is
    emulated — the copy, not blobfuse."""
    monkeypatch.setattr(fs, "_mount_root", lambda mp: str(tmp_path / mp.lstrip("/")))
    monkeypatch.setattr(fs, "ls", lambda p: [_entry(p + "/a.txt", "a.txt", False)])
    monkeypatch.setattr(fs, "read", lambda p: b"payload")
    assert fs.mount("abfss://ws@host/c", "/mydata") is True
    local = fs.getMountPath("/mydata")
    with open(local + "/a.txt", "rb") as fh:
        assert fh.read() == b"payload"


def test_mounting_the_same_point_twice_is_refused(http, monkeypatch, clean_mounts, tmp_path):
    """Fabric's own guidance is to check `mounts()` first; re-mounting is the
    caller's error, not something to silently redo."""
    monkeypatch.setattr(fs, "_mount_root", lambda mp: str(tmp_path / mp.lstrip("/")))
    monkeypatch.setattr(fs, "ls", lambda p: [])
    fs.mount("abfss://ws@host/c", "/mydata")
    with pytest.raises(ValueError, match="already mounted"):
        fs.mount("abfss://ws@host/other", "/mydata")


def test_mounts_lists_what_is_mounted_with_fabrics_attribute_names(
        http, monkeypatch, clean_mounts, tmp_path):
    """Fabric's example loops `any(m.mountPoint == p for m in mounts())`, so
    the attribute name is part of the contract."""
    monkeypatch.setattr(fs, "_mount_root", lambda mp: str(tmp_path / mp.lstrip("/")))
    monkeypatch.setattr(fs, "ls", lambda p: [])
    fs.mount("abfss://ws@host/c", "/mydata")
    entries = fs.mounts()
    assert [m.mountPoint for m in entries] == ["/mydata"]
    assert entries[0].source == "abfss://ws@host/c"


def test_unmount_removes_the_point_and_reports_whether_there_was_one(
        http, monkeypatch, clean_mounts, tmp_path):
    monkeypatch.setattr(fs, "_mount_root", lambda mp: str(tmp_path / mp.lstrip("/")))
    monkeypatch.setattr(fs, "ls", lambda p: [])
    fs.mount("abfss://ws@host/c", "/mydata")
    assert fs.unmount("/mydata") is True
    assert fs.mounts() == []
    assert fs.unmount("/mydata") is False, "unmounting nothing is False, not an error"


def test_getMountPath_on_an_unmounted_point_says_so(clean_mounts):
    with pytest.raises(KeyError, match="not mounted"):
        fs.getMountPath("/nope")


def test_a_single_file_source_mounts_as_that_file(
        http, monkeypatch, clean_mounts, tmp_path):
    def ls_fails(_p):
        raise RuntimeError("not a directory")

    monkeypatch.setattr(fs, "_mount_root", lambda mp: str(tmp_path / mp.lstrip("/")))
    monkeypatch.setattr(fs, "ls", ls_fails)
    monkeypatch.setattr(fs, "read", lambda p: b"one")
    fs.mount("abfss://ws@host/c/solo.txt", "/single")
    with open(fs.getMountPath("/single") + "/solo.txt", "rb") as fh:
        assert fh.read() == b"one"


def test_mount_roots_are_per_session(monkeypatch):
    """Two sessions in one agent must not share a mount directory — the same
    isolation contract 5 asserts for catalogs, applied to the filesystem."""
    monkeypatch.setenv("FABRIC_JOB_ID", "job-a")
    a = fs._mount_root("/m")
    monkeypatch.setenv("FABRIC_JOB_ID", "job-b")
    assert fs._mount_root("/m") != a


# --- lakehouse ----------------------------------------------------------------


def test_lakehouse_get_resolves_a_name_through_the_listing(http):
    """The documented lookup is BY NAME. The shim took `lakehouseId`, which is
    both the wrong parameter name and the wrong lookup."""
    http.push({"value": [{"id": LH, "displayName": "sales"}]})
    http.push({"id": LH})
    lakehouse.get("sales")
    assert http.last()["url"].endswith(f"/lakehouses/{LH}")


def test_lakehouse_get_with_no_name_uses_the_attached_lakehouse(http):
    http.push({"id": LH})
    lakehouse.get()
    assert http.last()["url"].endswith(f"/lakehouses/{LH}")


def test_an_unknown_lakehouse_name_is_named_in_the_error(http):
    http.push({"value": [{"id": LH, "displayName": "sales"}]})
    with pytest.raises(KeyError, match="marketing"):
        lakehouse.get("marketing")


def test_lakehouse_create_passes_the_schema_definition_through(http):
    lakehouse.create("lake", "desc", {"enableSchemas": True})
    assert http.last()["body"]["creationPayload"] == {"enableSchemas": True}


def test_lakehouse_list_honours_maxResults(http):
    http.push({"value": [{"id": str(n)} for n in range(10)]})
    assert len(lakehouse.list(maxResults=3)) == 3


def test_lakehouse_update_renames(http):
    http.push({"id": LH})
    lakehouse.update(LH, "newname", "why")
    assert http.last()["method"] == "PATCH"
    assert http.last()["body"] == {"displayName": "newname", "description": "why"}


def test_lakehouse_delete_reports_success(http):
    assert lakehouse.delete(LH) is True
    assert http.last()["method"] == "DELETE"


def test_listTables_reads_the_Tables_folder_not_a_metastore(http, monkeypatch):
    """Nothing in this stack holds a metastore — the lakehouse's Tables/ folder
    IS the table list, which is what livy_catalog enumerates on bind."""
    monkeypatch.setattr(fs, "ls", lambda p: [
        _entry(p + "/events", "events", True),
        _entry(p + "/_stray.txt", "_stray.txt", False),
    ])
    tables = lakehouse.listTables(LH)
    assert [t.name for t in tables] == ["events"], "a loose file is not a table"


def test_loadTable_refuses_by_name_rather_than_faking_a_load(http):
    """The plausible shortcut — read the CSV here, write a Delta table from the
    client — would be a DIFFERENT operation wearing the same name: no job, no
    server-side schema inference, and a silent success for options it never
    applied."""
    with pytest.raises(NotImplementedError) as exc:
        lakehouse.loadTable({"relativePath": "Files/x.csv"}, "t")
    assert "saveAsTable" in str(exc.value), "a refusal must name the way through"


# --- credentials: the write half, and the clock -------------------------------


def test_putSecret_writes_to_the_vault(http):
    """`getSecret` has been here since the shim started and this had not — the
    shape of gap contract 2 finds: a framework that manages its own secrets
    introspects for `putSecret`, sees nothing, and declines."""
    http.push({"value": "s3cret"})
    assert credentials.putSecret("myvault", "api-key", "s3cret") == "s3cret"
    call = http.last()
    assert call["method"] == "PUT"
    assert call["url"].endswith("/secrets/api-key?api-version=7.4")
    assert call["body"] == {"value": "s3cret"}


def _jwt(exp):
    import base64
    import json

    payload = base64.urlsafe_b64encode(json.dumps({"exp": exp}).encode()).rstrip(b"=")
    return "hdr." + payload.decode() + ".sig"


def test_isValidToken_reads_the_clock():
    import time

    assert credentials.isValidToken(_jwt(time.time() + 600)) is True
    assert credentials.isValidToken(_jwt(time.time() - 600)) is False


def test_an_unparseable_token_is_not_valid():
    """Unreadable is a "no": the caller's next move on False — mint a fresh
    one — is the safe one."""
    assert credentials.isValidToken("not-a-jwt") is False
    assert credentials.isValidToken("") is False
    assert credentials.isValidToken("a.!!!.c") is False


def test_a_token_without_exp_is_not_valid():
    """Real Entra always mints `exp`. Treating its absence as eternal is what
    internal/auth refuses to do, for the same reason."""
    import base64
    import json

    payload = base64.urlsafe_b64encode(json.dumps({"sub": "x"}).encode()).rstrip(b"=")
    assert credentials.isValidToken("h." + payload.decode() + ".s") is False


# --- session ------------------------------------------------------------------


def test_session_stop_asks_the_agent_rather_than_exiting(http, monkeypatch):
    """A sys.exit() here would take every other live notebook down with it —
    the shared-agent leak contract 5 exists to catch, in its worst form."""
    monkeypatch.setenv("SPARK_AGENT_URL", "http://agent:8099")
    monkeypatch.setenv("FABRIC_SESSION_ID", "sess-1")
    session.stop()
    assert http.last()["url"] == "http://agent:8099/close"
    assert http.last()["body"] == {"session": "sess-1", "detach": True}


def test_session_stop_passes_detach_through(http, monkeypatch):
    """In a high-concurrency session `detach` decides whether this notebook
    leaves or the shared session dies under its siblings."""
    monkeypatch.setenv("SPARK_AGENT_URL", "http://agent:8099")
    session.stop(detach=False)
    assert http.last()["body"]["detach"] is False


def test_restartPython_keeps_the_spark_context(http, monkeypatch):
    """The distinction is the point: restarting the engine too would cost every
    cached DataFrame and temp view for no reason."""
    monkeypatch.setenv("SPARK_AGENT_URL", "http://agent:8099")
    session.restartPython()
    assert http.last()["url"].endswith("/restart-python")


def test_session_outside_a_notebook_says_so(http, monkeypatch):
    """Rather than pretending to have stopped something."""
    monkeypatch.delenv("SPARK_AGENT_URL", raising=False)
    monkeypatch.delenv("NOTEBOOKUTILS_AGENT_URL", raising=False)
    with pytest.raises(RuntimeError, match="outside a notebook session"):
        session.stop()


# --- udf ----------------------------------------------------------------------


UDF_ITEM = {"id": "33333333-3333-3333-3333-333333333333", "displayName": "Validators"}

# The documented definition parts, verbatim in shape:
# rest/api/fabric/articles/item-management/definitions/user-data-function-definition
UDF_SCRIPT = """
import fabric.functions as fn

udf = fn.UserDataFunctions()

@udf.function()
def validate(schema: str, path: str = "default.csv") -> str:
    return f"{schema}:{path}"
"""

UDF_DEFINITION = {
    "runtime": "PYTHON",
    "connectedDataSources": [
        {"alias": "lh1", "artifactType": "Lakehouse", "artifactId": "a-lake"},
    ],
    "functions": [{"name": "validate", "description": "checks a schema"}],
}

UDF_METADATA = {
    "runtime": "PYTHON",
    "functionsMetadata": [{
        "name": "validate",
        "scriptFile": "function_app.py",
        "bindings": [
            {"type": "HttpTrigger", "name": "req", "direction": "In"},
            {"type": "FabricItem", "alias": "lh1", "name": "myLakehouse",
             "direction": "In"},
        ],
        "fabricProperties": {
            "fabricFunctionParameters": [{"name": "schema", "dataType": "str"},
                                         {"name": "path", "dataType": "str"}],
            "fabricFunctionReturnType": "str",
        },
    }],
}


def _b64(text):
    import base64
    return base64.b64encode(text.encode()).decode()


def _udf_definition_response():
    return {"definition": {"parts": [
        {"path": "definition.json", "payload": _b64(json.dumps(UDF_DEFINITION))},
        {"path": "resources/functions.json", "payload": _b64(json.dumps(UDF_METADATA))},
        {"path": "function_app.py", "payload": _b64(UDF_SCRIPT)},
    ]}}


def _load_udf(http):
    http.push({"value": [UDF_ITEM]})
    http.push(_udf_definition_response())
    return udf.getFunctions("Validators")


def test_getFunctions_reads_the_items_real_definition(http):
    """Not an invoke endpoint that does not exist — the item's DEFINITION is
    the documented source of its functions."""
    fns = _load_udf(http)
    assert http.calls[-1]["url"].endswith("/getDefinition")
    assert [f["Name"] for f in fns.functionDetails] == ["validate"]


def test_a_function_runs_its_own_code(http):
    """The item's real `function_app.py`, executed. A shim that returned a
    placeholder would be a plausible success — the exact failure mode
    `loadTable` refuses rather than commit."""
    fns = _load_udf(http)
    assert fns.validate(schema="sales", path="a.csv") == "sales:a.csv"


def test_positional_arguments_are_matched_to_the_declared_order(http):
    """Scala and R can only pass positional, so the item's own parameter list
    is what resolves them."""
    fns = _load_udf(http)
    assert fns.validate("sales", "a.csv") == "sales:a.csv"


def test_omitting_a_defaulted_parameter_is_a_legal_call(http):
    """The page documents that a parameter with a default may be omitted, so
    fewer args than declared is a legal call, not a length mismatch."""
    fns = _load_udf(http)
    assert fns.validate("sales") == "sales:default.csv"


def test_function_details_carry_parameters_return_type_and_connections(http):
    """Fabric documents all four, and its own examples read them to decide
    whether to call at all."""
    fns = _load_udf(http)
    detail = fns.functionDetails[0]
    assert [p["name"] for p in detail["Parameters"]] == ["schema", "path"]
    assert detail["FunctionReturnType"] == "str"
    assert detail["Description"] == "checks a schema"
    assert detail["DataSourceConnections"][0]["artifactType"] == "Lakehouse"


def test_item_details_carry_the_documented_keys(http):
    fns = _load_udf(http)
    assert fns.itemDetails["Name"] == "Validators"
    assert fns.itemDetails["Id"] == UDF_ITEM["id"]
    assert "WorkspaceId" in fns.itemDetails


def test_the_registration_decorators_are_honoured_not_faked(http):
    """`@udf.function()` and `@udf.connection(...)` are REGISTRATION on Fabric,
    not behaviour — returning the function unchanged is a faithful stand-in.
    If the stub dropped the decorated function, this call would not resolve."""
    fns = _load_udf(http)
    assert callable(fns.validate)


def test_a_function_declared_but_not_implemented_says_which(http):
    """definition.json can name a function `function_app.py` never defines —
    the docs say so explicitly ("you must implement them here")."""
    http.push({"value": [UDF_ITEM]})
    resp = _udf_definition_response()
    resp["definition"]["parts"][0]["payload"] = _b64(json.dumps({
        "functions": [{"name": "ghost", "description": ""}]}))
    http.push(resp)
    fns = udf.getFunctions("Validators")
    with pytest.raises(AttributeError, match=r"function_app\.py defines no such function"):
        fns.ghost()


def test_a_function_the_item_does_not_have_names_the_ones_it_does(http):
    """Fabric's own example checks `functionDetails` before invoking; the error
    should tell you what that check would have."""
    fns = _load_udf(http)
    with pytest.raises(AttributeError, match="validate"):
        fns.noSuchFunction()


def test_an_unknown_udf_item_is_named_in_the_error(http):
    http.push({"value": []})
    with pytest.raises(KeyError, match="Missing"):
        udf.getFunctions("Missing")


def test_a_udf_object_says_what_it_holds(http):
    fns = _load_udf(http)
    assert "Validators" in repr(fns) and "1 function" in repr(fns)


def test_running_a_udf_does_not_leave_fabric_functions_installed(http):
    """The stub is scoped to the call. Leaving `fabric.functions` in
    sys.modules would let a notebook's own `import fabric.functions` silently
    resolve to a stand-in that does nothing."""
    import sys
    before = sys.modules.get("fabric.functions")
    fns = _load_udf(http)
    fns.validate("s")
    assert sys.modules.get("fabric.functions") is before


def test_mkdirs_creates_a_directory_resource(http):
    fs.mkdirs("Files/new/deep")
    assert http.last()["url"].endswith("?resource=directory")
    assert http.last()["method"] == "PUT"


def test_a_scope_that_is_already_a_scope_is_not_suffixed_twice():
    """`storage` becomes `https://storage.azure.com/.default`; a caller who
    passed the full scope must not get `/.default/.default`."""
    assert credentials._scope("https://vault.azure.net/.default").endswith("/.default")
    assert credentials._scope("https://vault.azure.net/.default").count("/.default") == 1


def test_a_vault_passed_as_a_full_url_is_used_as_given(monkeypatch):
    """Fabric's own examples pass `https://<name>.vault.azure.net/`, and a
    shim that appended the DNS suffix to that would build a nonsense host."""
    no_override = type("C", (), {"vault_url": None})()
    monkeypatch.setattr(credentials, "config", lambda: no_override)
    assert credentials._vault_url("https://mine.vault.azure.net/") == \
        "https://mine.vault.azure.net"
    assert credentials._vault_url("mine") == "https://mine.vault.azure.net"


def test_a_connection_decorated_function_is_still_registered(http):
    """`@udf.connection(...)` wraps `@udf.function()`. If the stub's
    `connection` dropped the function, a lakehouse-bound UDF would vanish from
    the item — the shape most real UDFs have."""
    http.push({"value": [UDF_ITEM]})
    resp = _udf_definition_response()
    resp["definition"]["parts"][2]["payload"] = _b64('''
import fabric.functions as fn
udf = fn.UserDataFunctions()

@udf.connection(argName="myLakehouse", alias="lh1")
@udf.function()
def validate(schema: str, path: str = "d.csv") -> str:
    return "bound:" + schema
''')
    http.push(resp)
    fns = udf.getFunctions("Validators")
    assert fns.validate("sales") == "bound:sales"


def test_a_definition_missing_a_part_does_not_crash_the_read(http):
    """An item with no metadata part still lists its functions — the parts are
    documented as separately required, and a missing one should narrow what is
    known, not fail the read."""
    http.push({"value": [UDF_ITEM]})
    http.push({"definition": {"parts": [
        {"path": "definition.json", "payload": _b64(json.dumps(UDF_DEFINITION))},
        {"path": "function_app.py", "payload": _b64(UDF_SCRIPT)},
    ]}})
    fns = udf.getFunctions("Validators")
    assert fns.functionDetails[0]["Parameters"] == []
    assert fns.validate(schema="s", path="p") == "s:p"


def test_udf_restores_a_real_fabric_functions_module_if_one_was_installed(http, monkeypatch):
    """A caller that genuinely has `fabric.functions` installed must get theirs
    back — the stub is borrowed for the call, not substituted for the process."""
    import sys
    import types
    real = types.ModuleType("fabric.functions")
    monkeypatch.setitem(sys.modules, "fabric.functions", real)
    fns = _load_udf(http)
    fns.validate("s")
    assert sys.modules["fabric.functions"] is real


def test_a_missing_workspace_is_named_rather_than_guessed(monkeypatch):
    """Every module that can default a workspace says so when it cannot —
    rather than building a URL with an empty segment and failing at the far
    end, where the cause is no longer visible."""
    empty = type("C", (), {"workspace_id": None, "lakehouse_id": None})()
    monkeypatch.setattr(lakehouse, "config", lambda: empty)
    monkeypatch.setattr(udf, "config", lambda: empty)
    with pytest.raises(RuntimeError, match="no workspace"):
        lakehouse.list()
    with pytest.raises(RuntimeError, match="no workspace"):
        udf.getFunctions("anything")


def test_no_name_and_no_attached_lakehouse_says_which_is_missing(http, monkeypatch):
    no_lake = type("C", (), {"workspace_id": WS, "lakehouse_id": None,
                             "fabric_url": "https://localhost:9443"})()
    monkeypatch.setattr(lakehouse, "config", lambda: no_lake)
    monkeypatch.delenv("NOTEBOOKUTILS_LAKEHOUSE_ID", raising=False)
    with pytest.raises(RuntimeError, match="no default lakehouse is attached"):
        lakehouse.get()


def test_rm_of_a_directory_is_recursive_only_when_asked(http):
    fs.rm("Files/dir")
    assert "recursive=true" not in http.last()["url"]
    fs.rm("Files/dir", recurse=True)
    assert "recursive=true" in http.last()["url"]


def test_rm_of_a_non_empty_directory_raises_the_filesystem_error(http):
    """ADLS answers 409 DirectoryNotEmpty; a notebook author catches OSError.

    Translated rather than surfaced as an HTTP error for the same reason `mv`
    raises FileExistsError: the mapping is exact — `os.rmdir` raises ENOTEMPTY
    for this identical situation — and a notebook that catches OSError around a
    delete is ordinary code that should not need to know a REST status.
    """
    http.push(HttpError(409, "DirectoryNotEmpty", "u"))
    with pytest.raises(OSError) as caught:
        fs.rm("Files/dir")
    assert caught.value.errno == errno.ENOTEMPTY
    assert "recurse=True" in str(caught.value)


def test_rm_does_not_swallow_other_http_failures(http):
    """The 409 branch must not become a catch-all: a 403 is not an empty
    directory, and reporting it as one would send the caller to the wrong fix.
    """
    http.push(HttpError(403, "AuthorizationFailure", "u"))
    with pytest.raises(HttpError) as caught:
        fs.rm("Files/dir")
    assert caught.value.status == 403


def test_mv_of_a_directory_copies_the_tree(http, monkeypatch):
    monkeypatch.setattr(fs, "exists", lambda p: False)
    monkeypatch.setattr(fs, "_is_dir", lambda p: True)
    seen = {}
    monkeypatch.setattr(fs, "cp",
                        lambda src, dest, recurse=False: seen.update(recurse=recurse))
    monkeypatch.setattr(fs, "mkdirs", lambda p: None)
    monkeypatch.setattr(fs, "rm", lambda p, recurse=False: None)
    fs.mv("Files/src", "Files/dst")
    assert seen == {"recurse": True}, "a directory move must take the whole tree"


def test_is_dir_reads_the_resource_type_header(http):
    http.push((200, {"x-ms-resource-type": "directory"}, b""))
    assert fs._is_dir("Files/d") is True
    http.push((200, {"x-ms-resource-type": "file"}, b""))
    assert fs._is_dir("Files/a.txt") is False


def test_is_dir_treats_unreadable_as_not_a_directory(http):
    """Unreadable is not evidence of a directory — and guessing "yes" would
    send `mv` down the tree-copy path against a single file."""
    boom = Exception("denied")
    boom.status = 403
    http.push(boom)
    assert fs._is_dir("Files/a.txt") is False


def test_cell_context_tags_io_only_inside_a_notebook_run(monkeypatch):
    """Observed lineage with no parsing of user code — and absent outside a
    run, where there is no cell to attribute anything to."""
    monkeypatch.setenv("FABRIC_JOB_ID", "job-1")
    monkeypatch.setenv("FABRIC_CELL_INDEX", "3")
    assert fs._cell_context() == {"x-ms-fabric-job-id": "job-1",
                                  "x-ms-fabric-cell-index": "3"}
    monkeypatch.delenv("FABRIC_CELL_INDEX")
    assert fs._cell_context() == {}


def test_mount_entries_and_tables_are_readable_at_a_glance():
    m = fs.MountPointInfo("abfss://ws@host/c", "/mydata", "/tmp/x")
    assert "/mydata" in repr(m) and "abfss://ws@host/c" in repr(m)
    t = lakehouse.Table("events", "abfss://ws@host/lh/Tables/events")
    assert "events" in repr(t) and "delta" in repr(t)


def test_materialise_recurses_into_subdirectories(
        http, monkeypatch, clean_mounts, tmp_path):
    tree = {
        "abfss://ws@host/c": [_entry("abfss://ws@host/c/sub", "sub", True)],
        "abfss://ws@host/c/sub": [_entry("abfss://ws@host/c/sub/deep.txt", "deep.txt", False)],
    }
    monkeypatch.setattr(fs, "_mount_root", lambda mp: str(tmp_path / mp.lstrip("/")))
    monkeypatch.setattr(fs, "ls", lambda p: tree.get(p, []))
    monkeypatch.setattr(fs, "read", lambda p: b"deep")
    fs.mount("abfss://ws@host/c", "/m")
    with open(fs.getMountPath("/m") + "/sub/deep.txt", "rb") as fh:
        assert fh.read() == b"deep"


def test_getWithProperties_is_a_read_of_the_same_item(http):
    """Kept as a separate member because a framework introspects for it, and
    because Fabric documents the two as different reads."""
    http.push({"id": LH, "properties": {"sqlEndpointProperties": {}}})
    assert lakehouse.getWithProperties(LH)["id"] == LH


# --- where a session's workspace comes from ----------------------------------
#
# THE ORDER IS THE BUG THAT WAS THERE. Every `_ws` read the environment
# fallback and nothing else, so every notebook.* / lakehouse.* / udf.* /
# variableLibrary call needing a workspace failed inside a CORRECT Fabric
# session — the one where docs/38 contract 1 requires that fallback to be
# UNSET, because a fallback that can answer hides a broken context.
#
# Found by the %run e2e: seven contracts green, %run dead with "no workspace".
# Contract 2 grades signatures, so it could never have caught this.


def test_the_context_answers_before_the_environment_fallback(monkeypatch):
    from notebookutils import _config
    monkeypatch.setattr(_config.runtime if hasattr(_config, "runtime") else runtime,
                        "context", {"currentWorkspaceId": "from-context"},
                        raising=False)
    monkeypatch.setattr(runtime, "context", {"currentWorkspaceId": "from-context"},
                        raising=False)
    assert _config.session_workspace_id("from-env") == "from-context"


def test_the_fallback_answers_when_there_is_no_context(monkeypatch):
    """Outside a notebook there is no context, and the env variable is then the
    only thing that can say — which is what it is for."""
    from notebookutils import _config
    monkeypatch.setattr(runtime, "context", {}, raising=False)
    assert _config.session_workspace_id("from-env") == "from-env"


def test_an_unreadable_context_is_not_an_error_just_no_answer(monkeypatch):
    """A context that raises must not take down a call the fallback could have
    served."""
    from notebookutils import _config

    class Boom:
        def get(self, _k):
            raise RuntimeError("no session")

    monkeypatch.setattr(runtime, "context", Boom(), raising=False)
    assert _config.session_workspace_id("from-env") == "from-env"


def test_notebook_resolves_its_workspace_from_the_context(http, monkeypatch):
    """The end the %run e2e actually hit: a notebook call with the env
    fallback absent, which is the correct-session case."""
    from notebookutils import notebook
    no_env = type("C", (), {"fabric_url": "https://localhost:9443",
                            "workspace_id": None})()
    monkeypatch.setattr(notebook, "config", lambda: no_env)
    # `notebook` is not in the shared fixture's stub list; stub it here rather
    # than widening a fixture every other test depends on.
    monkeypatch.setattr(notebook, "request", http)
    monkeypatch.setattr(runtime, "context", {"currentWorkspaceId": WS},
                        raising=False)
    http.push({"value": []})
    notebook.list()
    assert f"/workspaces/{WS}/" in http.last()["url"], http.last()["url"]
