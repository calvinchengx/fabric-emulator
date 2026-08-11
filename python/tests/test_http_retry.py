"""`_http.request` retries only where a retry cannot duplicate work.

WHY THIS EXISTS. MEASURED against real Fabric on 2026-08-11: polling a job
instance every 0.3s, 1 request in 25 came back `[Errno 61] Connection refused`,
reproducibly and at a different poll each time. The client had NO retry at all,
so one transient refusal killed a whole notebook run partway through.

Against the emulator on loopback a refusal essentially never happens, so the
missing retry had no local symptom — it looked perfect here and died on a
tenant. That is the emulator-green/tenant-broken direction, and it lived in a
SHIPPED wheel rather than in an example, which is the worse place for it.

THE ASYMMETRY THAT SHAPES THE POLICY. Retrying is only safe when the request
provably was not processed. A refused connection was never established, and
429/503 are the service explicitly declining — none of those can have run the
job. A 500/502/504 or a read timeout arrives AFTER the request reached the
service, so retrying a POST could submit the same notebook twice. A duplicate
run is worse than a surfaced error, so those are not retried.
"""
import email.message
import io
import urllib.error

import pytest
from notebookutils import _http


class _FakeResponse(io.BytesIO):
    def __init__(self, payload=b'{"ok": true}', status=200, headers=None):
        super().__init__(payload)
        self.status = status
        self.headers = headers or {}

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()
        return False


@pytest.fixture
def wire(monkeypatch):
    """Script urlopen's outcomes; record every sleep instead of taking it."""
    slept = []
    monkeypatch.setattr(_http.time, "sleep", slept.append)
    monkeypatch.setattr(_http, "config", lambda: type("C", (), {"ssl_context": lambda self: None})())

    def install(outcomes):
        calls = []

        def fake_urlopen(req, context=None):
            calls.append(req)
            outcome = outcomes[min(len(calls) - 1, len(outcomes) - 1)]
            if isinstance(outcome, Exception):
                raise outcome
            return outcome

        monkeypatch.setattr(_http.urllib.request, "urlopen", fake_urlopen)
        return calls, slept

    return install


def _refused():
    return urllib.error.URLError(ConnectionRefusedError(61, "Connection refused"))


def _headers(pairs=None):
    """A real `email.message.Message`, which is what urllib hands back.

    NOT a dict: a Message looks its keys up CASE-INSENSITIVELY, and real Fabric
    sends `retry-after` in lower case. A dict fixture here would have passed
    while the production lookup depended on the exact casing.
    """
    m = email.message.Message()
    for k, v in (pairs or {}).items():
        m[k] = v
    return m


def _http_error(code, headers=None):
    return urllib.error.HTTPError(
        "https://x/y", code, "boom", _headers(headers), io.BytesIO(b"body"))


# --- the measured failure ----------------------------------------------------

def test_a_refused_connection_is_retried_and_then_succeeds(wire):
    calls, slept = wire([_refused(), _FakeResponse()])
    assert _http.request("GET", "https://x/y") == {"ok": True}
    assert len(calls) == 2, "the refused attempt was not retried"
    assert slept, "retried without waiting at all"


def test_a_refused_connection_is_retried_for_a_post_too(wire):
    """Safe precisely because the connection was never established: the service
    cannot have created a job it never received a byte of."""
    calls, _ = wire([_refused(), _refused(), _FakeResponse()])
    _http.request("POST", "https://x/y", body={"a": 1})
    assert len(calls) == 3


def test_it_gives_up_and_raises_rather_than_retrying_forever(wire):
    calls, _ = wire([_refused()])
    with pytest.raises(urllib.error.URLError):
        _http.request("GET", "https://x/y")
    assert len(calls) == _http._MAX_ATTEMPTS


# --- what must NOT be retried ------------------------------------------------

def test_a_read_timeout_is_not_retried(wire):
    """It arrives after the request reached the service, so a retried POST
    could run the notebook twice."""
    calls, _ = wire([urllib.error.URLError(TimeoutError("timed out")), _FakeResponse()])
    with pytest.raises(urllib.error.URLError):
        _http.request("POST", "https://x/y")
    assert len(calls) == 1


@pytest.mark.parametrize("code", [500, 502, 504])
def test_server_errors_after_the_request_landed_are_not_retried(wire, code):
    calls, _ = wire([_http_error(code), _FakeResponse()])
    with pytest.raises(_http.HttpError):
        _http.request("POST", "https://x/y")
    assert len(calls) == 1, f"{code} was retried; a POST could duplicate work"


def test_a_404_is_not_retried(wire):
    """Real Fabric answers 404 for `…/notebookRun`. Retrying it would turn one
    fast, correct answer into four slow ones."""
    calls, _ = wire([_http_error(404), _FakeResponse()])
    with pytest.raises(_http.HttpError):
        _http.request("GET", "https://x/y")
    assert len(calls) == 1


# --- Retry-After -------------------------------------------------------------

@pytest.mark.parametrize("code", [429, 503])
def test_a_declined_request_is_retried(wire, code):
    calls, _ = wire([_http_error(code), _FakeResponse()])
    assert _http.request("POST", "https://x/y") == {"ok": True}
    assert len(calls) == 2


def test_retry_after_is_honoured_rather_than_second_guessed(wire):
    """Real Fabric sent `retry-after: 20` on an operation that finished in 13s,
    so it is a floor the service ASKS for, not an estimate to improve on."""
    calls, slept = wire([_http_error(429, {"Retry-After": "20"}), _FakeResponse()])
    _http.request("GET", "https://x/y")
    assert len(calls) == 2
    assert slept == [20.0], f"slept {slept}, want the 20s the service asked for"


def test_retry_after_is_capped_so_one_header_cannot_hang_a_notebook(wire):
    calls, slept = wire([_http_error(503, {"Retry-After": "86400"}), _FakeResponse()])
    _http.request("GET", "https://x/y")
    assert slept == [float(_http._MAX_RETRY_AFTER)]
    assert len(calls) == 2


def test_the_lower_case_spelling_a_tenant_actually_sends_is_honoured(wire):
    """Real Fabric sent `retry-after: 20`, lower case. urllib's Message is
    case-insensitive, so this passes either way — the test exists so that
    swapping in a case-sensitive mapping cannot silently disable the header."""
    calls, slept = wire([_http_error(503, {"retry-after": "7"}), _FakeResponse()])
    _http.request("GET", "https://x/y")
    assert len(calls) == 2
    assert slept == [7.0], f"lower-case retry-after ignored: {slept}"


def test_an_http_date_retry_after_falls_back_instead_of_crashing(wire):
    """The HTTP-date form is legal and rare. Parsing it wrongly would sleep for
    years; failing on it would turn a retryable answer into an error."""
    calls, slept = wire([
        _http_error(429, {"Retry-After": "Wed, 21 Oct 2026 07:28:00 GMT"}),
        _FakeResponse()])
    _http.request("GET", "https://x/y")
    assert len(calls) == 2
    assert slept and all(0 < s <= _http._MAX_RETRY_AFTER for s in slept), slept


def test_backoff_grows_rather_than_hammering_at_a_fixed_rate(wire):
    _, slept = wire([_refused()])
    with pytest.raises(urllib.error.URLError):
        _http.request("GET", "https://x/y")
    assert slept == sorted(slept) and len(set(slept)) > 1, f"not backing off: {slept}"


# --- the happy path is untouched ---------------------------------------------

def test_a_successful_call_neither_sleeps_nor_repeats(wire):
    """The cost of the retry loop on the overwhelmingly common path is zero."""
    calls, slept = wire([_FakeResponse()])
    assert _http.request("GET", "https://x/y") == {"ok": True}
    assert len(calls) == 1 and slept == []


def test_raw_still_returns_status_headers_and_bytes(wire):
    wire([_FakeResponse(b"xyz", 202, {"Location": "/op/1"})])
    status, headers, payload = _http.request("POST", "https://x/y", raw=True)
    assert (status, headers["Location"], payload) == (202, "/op/1", b"xyz")
