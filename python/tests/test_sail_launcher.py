"""The Sail launcher's token-refresh arithmetic.

The launcher had no tests, and it grew a real calculation the day a scheduled
notebook run started failing with `401 Unauthorized` raised inside a cell: the
emulator's clock had been advanced (the only way to test a schedule without
waiting for a real occurrence), so a token the issuer considered fresh was
judged expired by the service reading it, while the launcher slept on a
deadline it had computed before the jump.

What matters here is that the deadline moves EARLIER by the offset, that a
clock nobody can read is treated as no offset at all, and that an advance
larger than a token's life cannot spin the supervisor into a restart loop.
"""

import importlib.util
import pathlib
import sys

import pytest

LAUNCHER = (
    pathlib.Path(__file__).resolve().parents[2] / "docker" / "sail" / "launcher.py"
)


def load():
    """Import launcher.py by path — it ships as a script, not a package."""
    spec = importlib.util.spec_from_file_location("sail_launcher", LAUNCHER)
    mod = importlib.util.module_from_spec(spec)
    sys.modules["sail_launcher"] = mod
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture
def launcher():
    return load()


def test_without_skew_the_deadline_is_lifetime_minus_margin(launcher):
    at = 1_000_000.0
    assert launcher.refresh_deadline(at, 3600, 0, margin=300) == at + 3300


def test_an_advanced_clock_pulls_the_deadline_earlier_by_the_offset(launcher):
    # The failure this file exists for: 3600s token, 300s margin, and an
    # emulator advanced 1200s. Refreshing at 3300s leaves an 1100s window in
    # which the launcher believes the token is good and every call 401s.
    at = 1_000_000.0
    assert launcher.refresh_deadline(at, 3600, 1200, margin=300) == at + 2100


def test_a_negative_offset_is_ignored_rather_than_extending_the_token(launcher):
    # A clock BEHIND real time cannot make a token live longer than the issuer
    # said; treating it as extra life would be the same bug pointing backwards.
    at = 1_000_000.0
    assert launcher.refresh_deadline(at, 3600, -600, margin=300) == at + 3300


def test_an_advance_past_the_token_lifetime_floors_instead_of_looping(launcher):
    # Nothing can be minted that survives this, so the deadline must not go to
    # now-or-earlier: that would restart sail continuously while every call
    # failed anyway.
    at = 1_000_000.0
    assert launcher.refresh_deadline(at, 3600, 9999, margin=300) == at + 60


def test_an_ordinary_advance_is_not_reported_as_hopeless(launcher):
    # The case this whole fix exists for. A 3600s token under the 1200s advance
    # a schedule test makes is still accepted for 2400s — most of a working
    # session — so warning here would cry wolf on the normal path. Caught by
    # running it: the first version keyed off lifetime-minus-margin and
    # announced that 1200s "outruns the 3600s token lifetime", which is false.
    assert not launcher.hopeless(3600, 1200)
    assert not launcher.hopeless(3600, 3599)


def test_an_advance_past_the_lifetime_is_hopeless(launcher):
    assert launcher.hopeless(3600, 3600)
    assert launcher.hopeless(3600, 9999)


def test_no_skew_is_never_hopeless(launcher):
    assert not launcher.hopeless(3600, 0)
    assert not launcher.hopeless(3600, -50)


def test_the_emulator_origin_comes_from_the_storage_endpoint(launcher, monkeypatch):
    monkeypatch.delenv("FABRIC_EMULATOR_URL", raising=False)
    monkeypatch.setenv("AZURE_STORAGE_ENDPOINT", "https://fabric-emulator:9443/onelake")
    assert launcher.emulator_base() == "https://fabric-emulator:9443"


def test_an_explicit_url_wins_over_the_storage_endpoint(launcher, monkeypatch):
    monkeypatch.setenv("FABRIC_EMULATOR_URL", "https://elsewhere:1234/")
    monkeypatch.setenv("AZURE_STORAGE_ENDPOINT", "https://fabric-emulator:9443/onelake")
    assert launcher.emulator_base() == "https://elsewhere:1234"


def test_no_endpoint_means_no_origin(launcher, monkeypatch):
    monkeypatch.delenv("FABRIC_EMULATOR_URL", raising=False)
    monkeypatch.setenv("AZURE_STORAGE_ENDPOINT", "")
    assert launcher.emulator_base() is None


def test_an_unreachable_clock_reads_as_no_skew(launcher, monkeypatch):
    # Degrading to today's behaviour is the requirement: a clock endpoint that
    # is missing, slow or serving nonsense must never stop sail from starting.
    monkeypatch.setenv("FABRIC_EMULATOR_URL", "https://127.0.0.1:9/nope")
    assert launcher.clock_offset() == 0


def test_an_unconfigured_endpoint_reads_as_no_skew(launcher, monkeypatch):
    monkeypatch.delenv("FABRIC_EMULATOR_URL", raising=False)
    monkeypatch.setenv("AZURE_STORAGE_ENDPOINT", "")
    assert launcher.clock_offset() == 0


def test_the_offset_is_read_from_the_clock_endpoint(launcher, monkeypatch):
    import http.server
    import threading

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):  # noqa: N802 - BaseHTTPRequestHandler's interface
            body = b'{"frozen":false,"now":1786066201,"offset":1200}'
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *_):
            pass

    srv = http.server.HTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        monkeypatch.setenv("FABRIC_EMULATOR_URL", f"http://127.0.0.1:{srv.server_port}")
        assert launcher.clock_offset() == 1200
    finally:
        srv.shutdown()
