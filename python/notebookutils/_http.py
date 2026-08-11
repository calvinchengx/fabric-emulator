"""Tiny stdlib HTTP helper shared by the shim modules.

Deliberately stdlib-only: a notebookutils shim that pulled in requests/httpx
would drag its own TLS + dependency surface into every notebook kernel. urllib
is enough to speak the emulator's REST and DFS surfaces.
"""
import json
import time
import urllib.error
import urllib.request

from ._config import config

# Retry policy. MEASURED against real Fabric on 2026-08-11: polling a job
# instance every 0.3s, 1 request in 25 came back `[Errno 61] Connection
# refused` — reproducibly, at a different poll each time. Against the emulator
# on loopback that essentially never happens, so a client with no retry looks
# perfect locally and dies on a tenant partway through a long notebook. It is
# the same emulator-green/tenant-broken shape as a flat poll that ignores
# `Retry-After`, and it lived in a SHIPPED wheel rather than in an example.
_MAX_ATTEMPTS = 4
_BACKOFF_BASE = 0.5   # seconds; doubled per attempt
_MAX_RETRY_AFTER = 60  # cap: a server asking for longer is not worth blocking on

# Statuses we retry. BOTH mean the service declined to PROCESS the request, so
# retrying cannot duplicate work — which is why a non-idempotent POST is safe
# here. Deliberately NOT 500/502/504: those arrive after the request reached
# the service, so a retried POST could run the job twice. A duplicate notebook
# run is worse than a surfaced error.
_RETRY_STATUSES = {429, 503}


class HttpError(Exception):
    def __init__(self, status, body, url):
        super().__init__(f"{status} for {url}: {body[:200]}")
        self.status = status
        self.body = body
        self.url = url


def _retry_after_seconds(headers, attempt):
    """How long to wait before the next attempt.

    `Retry-After` is what the service ASKS for and is honoured when present —
    real Fabric sends `retry-after: 20` on operations that finish in 13s, so it
    is a floor it wants respected, not an estimate to second-guess. Without one,
    exponential backoff.
    """
    raw = (headers or {}).get("Retry-After") or (headers or {}).get("retry-after")
    if raw:
        try:
            return min(float(raw), _MAX_RETRY_AFTER)
        except (TypeError, ValueError):
            # An HTTP-date form is legal and rare; backoff rather than parse it
            # wrongly and sleep for a decade.
            pass
    return _BACKOFF_BASE * (2 ** attempt)


def _is_unprocessed(err):
    """True when the request provably never reached the service.

    A refused connection is the one network failure that is safe to retry for
    ANY method: the TCP connection was never established, so nothing was read,
    parsed or executed. A read timeout is NOT — the service may be mid-way
    through creating a job.
    """
    reason = getattr(err, "reason", None)
    return isinstance(reason, ConnectionRefusedError)


def request(method, url, *, token=None, body=None, headers=None, raw=False):
    """One HTTP round-trip, retried only where a retry cannot duplicate work.

    `body` is JSON-encoded unless bytes; returns the parsed JSON (or raw bytes
    when raw=True). Non-2xx raises HttpError.
    """
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

    for attempt in range(_MAX_ATTEMPTS):
        last = attempt == _MAX_ATTEMPTS - 1
        req = urllib.request.Request(url, data=data, method=method, headers=hdrs)
        try:
            with urllib.request.urlopen(req, context=config().ssl_context()) as r:
                payload = r.read()
                if raw:
                    return r.status, dict(r.headers), payload
                return json.loads(payload) if payload else {}
        except urllib.error.HTTPError as e:
            detail = e.read().decode("utf-8", "replace")
            if e.code in _RETRY_STATUSES and not last:
                time.sleep(_retry_after_seconds(e.headers, attempt))
                continue
            raise HttpError(e.code, detail, url) from None
        except urllib.error.URLError as e:
            if _is_unprocessed(e) and not last:
                time.sleep(_BACKOFF_BASE * (2 ** attempt))
                continue
            raise
