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
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

from notebookutils import credentials, env, fs, lakehouse, runtime  # noqa: E402

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
    for mod in (fs, lakehouse, credentials):
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
    for mod in (fs, lakehouse, credentials, runtime):
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
