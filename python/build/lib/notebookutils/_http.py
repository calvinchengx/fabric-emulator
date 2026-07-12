"""Tiny stdlib HTTP helper shared by the shim modules.

Deliberately stdlib-only: a notebookutils shim that pulled in requests/httpx
would drag its own TLS + dependency surface into every notebook kernel. urllib
is enough to speak the emulator's REST and DFS surfaces.
"""
import json
import urllib.error
import urllib.request

from ._config import config


class HttpError(Exception):
    def __init__(self, status, body, url):
        super().__init__(f"{status} for {url}: {body[:200]}")
        self.status = status
        self.body = body
        self.url = url


def request(method, url, *, token=None, body=None, headers=None, raw=False):
    """One HTTP round-trip. `body` is JSON-encoded unless bytes; returns the
    parsed JSON (or raw bytes when raw=True). Non-2xx raises HttpError."""
    hdrs = dict(headers or {})
    data = None
    if body is not None:
        if isinstance(body, (bytes, bytearray)):
            data = bytes(body)
        else:
            data = json.dumps(body).encode()
            hdrs.setdefault("Content-Type", "application/json")
    if token:
        hdrs["Authorization"] = "Bearer " + token
    req = urllib.request.Request(url, data=data, method=method, headers=hdrs)
    try:
        with urllib.request.urlopen(req, context=config().ssl_context()) as r:
            payload = r.read()
            if raw:
                return r.status, dict(r.headers), payload
            return json.loads(payload) if payload else {}
    except urllib.error.HTTPError as e:
        detail = e.read().decode("utf-8", "replace")
        raise HttpError(e.code, detail, url) from None
