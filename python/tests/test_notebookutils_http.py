"""The one HTTP round-trip every shim call goes through.

Deliberately stdlib-only — a notebookutils shim that pulled in requests would
drag its own TLS and dependency surface into every notebook kernel — which
means the request assembly is hand-rolled and worth pinning: the method, the
bearer, JSON-vs-bytes bodies, and the two return shapes (`raw=True` for the DFS
surface's headers, parsed JSON otherwise).

`urlopen` is stubbed. What is under test is what we SEND and how we read the
answer, not whether a server replies.
"""
import email.message
import io
import json
import pathlib
import sys
import urllib.error

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))

from notebookutils import _http  # noqa: E402


class FakeResponse(io.BytesIO):
    """Enough of http.client.HTTPResponse for the `with urlopen(...)` block."""

    def __init__(self, payload=b"", status=200, headers=None):
        super().__init__(payload)
        self.status = status
        self.headers = headers or {}

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


class Sent:
    """Attributes, not a dict: a dict holding a Request, a context and a list
    infers as a union, and `ty` rejects assigning into it."""

    def __init__(self):
        self.req = None
        self.context = None
        self.answers = []


def http_error(url, code, body, reason="err"):
    """A urllib HTTPError with the headers object typeshed expects.

    `{}` reads fine at runtime but is not an `email.message.Message`, and the
    type checker is right that it is not.
    """
    return urllib.error.HTTPError(url, code, reason, email.message.Message(), io.BytesIO(body))


@pytest.fixture
def sent(monkeypatch):
    """Capture the urllib Request and replay a canned response."""
    captured = Sent()

    def fake_urlopen(req, context=None):
        captured.req = req
        captured.context = context
        if captured.answers:
            a = captured.answers.pop(0)
            if isinstance(a, Exception):
                raise a
            return a
        return FakeResponse(b"{}")

    monkeypatch.setattr(_http.urllib.request, "urlopen", fake_urlopen)
    monkeypatch.setattr(_http, "config",
                        lambda: type("C", (), {"ssl_context": lambda self=None: "CTX"})())
    return captured


def push(sent, response):
    sent.answers.append(response)
    return sent


# --- what we send -------------------------------------------------------------

def test_the_method_and_url_reach_urllib(sent):
    _http.request("DELETE", "https://x/y")
    assert sent.req.get_method() == "DELETE"
    assert sent.req.full_url == "https://x/y"


def test_a_token_becomes_a_bearer_header(sent):
    _http.request("GET", "https://x/y", token="abc")
    assert sent.req.get_header("Authorization") == "Bearer abc"


def test_no_token_sends_no_authorization_header(sent):
    # The unauthenticated path is real: local Delta tables need no bearer, and
    # sending "Bearer None" would be a 401 that names nothing.
    _http.request("GET", "https://x/y")
    assert sent.req.get_header("Authorization") is None


def test_a_dict_body_is_json_encoded_with_a_content_type(sent):
    _http.request("POST", "https://x/y", body={"a": 1})
    assert json.loads(sent.req.data) == {"a": 1}
    assert sent.req.get_header("Content-type") == "application/json"


def test_a_bytes_body_is_sent_verbatim_without_a_json_content_type(sent):
    # OneLake appends raw bytes; JSON-encoding them would corrupt every upload.
    _http.request("PATCH", "https://x/y", body=b"\x00\x01raw")
    assert sent.req.data == b"\x00\x01raw"
    assert sent.req.get_header("Content-type") is None


def test_a_bytearray_body_is_also_sent_verbatim(sent):
    _http.request("PATCH", "https://x/y", body=bytearray(b"ab"))
    assert sent.req.data == b"ab"


def test_caller_headers_are_preserved_and_can_be_added_to(sent):
    _http.request("GET", "https://x/y", headers={"Host": "onelake"}, token="t")
    assert sent.req.get_header("Host") == "onelake"
    assert sent.req.get_header("Authorization") == "Bearer t"


def test_an_explicit_content_type_is_not_overwritten(sent):
    # setdefault, not assignment: a caller sending JSON under a different media
    # type keeps it.
    _http.request("POST", "https://x/y", body={"a": 1},
                  headers={"Content-Type": "application/merge-patch+json"})
    assert sent.req.get_header("Content-type") == "application/merge-patch+json"


def test_the_configured_ssl_context_is_used(sent):
    # Local certs are self-signed; a default context would refuse every call.
    _http.request("GET", "https://x/y")
    assert sent.context == "CTX"


# --- how we read the answer ---------------------------------------------------

def test_json_is_parsed_by_default(sent):
    push(sent, FakeResponse(b'{"value": [1, 2]}'))
    assert _http.request("GET", "https://x/y") == {"value": [1, 2]}


def test_an_empty_body_becomes_an_empty_dict_not_a_json_error(sent):
    # A 204 with no body is an ordinary answer from the control plane.
    push(sent, FakeResponse(b""))
    assert _http.request("DELETE", "https://x/y") == {}


def test_raw_returns_status_headers_and_bytes(sent):
    # The DFS surface needs the headers: `append` reads Content-Length to learn
    # where end-of-file is.
    push(sent, FakeResponse(b"payload", status=206, headers={"Content-Length": "7"}))
    status, headers, payload = _http.request("GET", "https://x/y", raw=True)
    assert (status, payload) == (206, b"payload")
    assert headers["Content-Length"] == "7"


# --- errors -------------------------------------------------------------------

def test_an_http_error_becomes_an_httperror_carrying_status_body_and_url(sent):
    push(sent, http_error("https://x/y", 409, b"already exists", "Conflict"))
    with pytest.raises(_http.HttpError) as ei:
        _http.request("PUT", "https://x/y")
    err = ei.value
    assert err.status == 409
    assert err.body == "already exists"
    assert err.url == "https://x/y"


def test_the_error_message_leads_with_the_status_and_url(sent):
    # This string is what a notebook user sees; a bare "HTTP Error" names
    # neither what failed nor where.
    push(sent, http_error("https://x/y", 403, b"no access", "Forbidden"))
    with pytest.raises(_http.HttpError, match=r"403 for https://x/y: no access"):
        _http.request("GET", "https://x/y")


def test_a_long_error_body_is_truncated_in_the_message_but_kept_whole(sent):
    # A multi-megabyte HTML error page must not become the exception message,
    # but the caller may still want it.
    body = b"x" * 5000
    push(sent, http_error("https://x/y", 500, body))
    with pytest.raises(_http.HttpError) as ei:
        _http.request("GET", "https://x/y")
    assert len(str(ei.value)) < 400
    assert len(ei.value.body) == 5000


def test_an_undecodable_error_body_does_not_mask_the_status(sent):
    # errors="replace": a binary error page must still surface as its status
    # rather than a UnicodeDecodeError from inside the shim.
    push(sent, http_error("https://x/y", 502, b"\xff\xfe\x00bad", "bad"))
    with pytest.raises(_http.HttpError) as ei:
        _http.request("GET", "https://x/y")
    assert ei.value.status == 502
