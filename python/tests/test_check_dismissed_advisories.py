"""The promise a dismissal makes, and the day it comes due.

A Dependabot dismissal is final — the alert never returns, whatever upstream
does. Dismissing "tolerable risk" with no fix available is therefore a decision
made once on a fact that was temporary, and nothing in GitHub notices when the
fact changes.

These exercise the day it changes. The fetcher is injected, so no test reaches
the network: what is under test is the reading of the answer, not GitHub's
ability to give one.
"""
import json
import pathlib
import sys
import urllib.error

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_dismissed_advisories as c  # noqa: E402

ENTRY = {"ghsa": "GHSA-x", "alert": 56, "ecosystem": "pip", "package": "mlflow",
         "reason": "tolerable_risk", "dismissed": "2026-09-01"}


def _advisory(patched=None, name="mlflow", eco="pip"):
    return {"vulnerabilities": [{
        "package": {"ecosystem": eco, "name": name},
        "vulnerable_version_range": ">= 3.13.0, <= 3.15.2",
        "first_patched_version": {"identifier": patched} if patched else None,
    }]}


def test_no_fix_upstream_leaves_the_dismissal_standing():
    assert c.review(ENTRY, _advisory()) == []


def test_a_patched_release_expires_the_dismissal():
    """The whole point: the day a fix lands, the reasoning is void."""
    found = c.review(ENTRY, _advisory(patched="3.15.3"))
    assert found and "FIXED UPSTREAM in 3.15.3" in found[0]
    assert "Re-open it" in found[0]


def test_the_failure_names_the_alert_to_re_open():
    """Useless without it — the alert number is the only way back to the thing
    that was dismissed."""
    msg = c.review(ENTRY, _advisory(patched="4.0.0"))[0]
    assert "56" in msg and "tolerable_risk" in msg


def test_an_advisory_that_no_longer_lists_the_package_is_flagged():
    """Withdrawn, re-scoped or renamed — all mean the statement the dismissal
    rested on has changed, and none should read as 'still fine'."""
    found = c.review(ENTRY, _advisory(name="something-else"))
    assert found and "no longer lists" in found[0]


def test_a_different_ecosystem_does_not_count_as_a_match():
    found = c.review(ENTRY, _advisory(eco="npm"))
    assert found and "no longer lists" in found[0]


def test_a_patched_version_given_as_a_bare_string_is_still_read():
    """The API returns an object today. Tolerating a bare string costs nothing
    and stops a shape change reading as 'no fix'."""
    adv = _advisory()
    adv["vulnerabilities"][0]["first_patched_version"] = "3.15.3"
    found = c.review(ENTRY, adv)
    assert found and "3.15.3" in found[0]


def test_a_network_failure_is_reported_not_swallowed(monkeypatch, tmp_path, capsys):
    """An unreachable API must not read as 'nothing has changed' — that is the
    silent pass this check exists to prevent."""
    f = tmp_path / "d.json"
    f.write_text(json.dumps({"dismissed": [ENTRY]}), encoding="utf-8")
    monkeypatch.setattr(c, "TRACKED", f)

    def boom(_ghsa):
        raise urllib.error.URLError("no route to host")

    assert c.main(fetcher=boom) == 1
    assert "unreviewed rather than confirmed" in capsys.readouterr().err


def test_main_passes_when_nothing_has_changed(monkeypatch, tmp_path, capsys):
    f = tmp_path / "d.json"
    f.write_text(json.dumps({"dismissed": [ENTRY]}), encoding="utf-8")
    monkeypatch.setattr(c, "TRACKED", f)
    assert c.main(fetcher=lambda _g: _advisory()) == 0
    assert "dismissal of alert 56 holds" in capsys.readouterr().out


def test_main_fails_once_a_fix_exists(monkeypatch, tmp_path, capsys):
    f = tmp_path / "d.json"
    f.write_text(json.dumps({"dismissed": [ENTRY]}), encoding="utf-8")
    monkeypatch.setattr(c, "TRACKED", f)
    assert c.main(fetcher=lambda _g: _advisory(patched="3.15.3")) == 1
    assert "FIXED UPSTREAM" in capsys.readouterr().err


def test_an_empty_list_passes_rather_than_erroring(monkeypatch, tmp_path, capsys):
    f = tmp_path / "d.json"
    f.write_text(json.dumps({"dismissed": []}), encoding="utf-8")
    monkeypatch.setattr(c, "TRACKED", f)
    assert c.main(fetcher=lambda _g: {}) == 0
    assert "nothing dismissed" in capsys.readouterr().out


def test_the_repositorys_own_file_is_well_formed():
    """Against the real file: every entry needs the fields the check reads, or
    it would raise KeyError in CI rather than report."""
    tracked = json.loads(c.TRACKED.read_text(encoding="utf-8"))["dismissed"]
    for e in tracked:
        for field in ("ghsa", "alert", "ecosystem", "package", "reason",
                      "dismissed", "why", "revisit_when"):
            assert field in e, f"{e.get('ghsa')} is missing {field}"


def _sent(monkeypatch, token):
    """Capture the Request `fetch` builds without sending it."""
    seen = {}

    class FakeResponse:
        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

        def read(self):
            return b'{"vulnerabilities": []}'

    def fake_urlopen(req, timeout=None):
        seen["headers"] = dict(req.header_items())
        seen["url"] = req.full_url
        return FakeResponse()

    monkeypatch.delenv("GITHUB_TOKEN", raising=False)
    monkeypatch.delenv("GH_TOKEN", raising=False)
    if token:
        monkeypatch.setenv("GITHUB_TOKEN", token)
    monkeypatch.setattr(c.urllib.request, "urlopen", fake_urlopen)
    c.fetch("GHSA-x")
    return seen


def test_fetch_asks_the_right_url_and_works_without_a_token(monkeypatch):
    """The endpoint is public — a laptop with no credentials must still get an
    answer, or the check is CI-only and nobody can reproduce a failure."""
    seen = _sent(monkeypatch, token=None)
    assert seen["url"].endswith("/advisories/GHSA-x")
    assert not any(k.lower() == "authorization" for k in seen["headers"])


def test_fetch_uses_a_token_when_one_is_present(monkeypatch):
    """Only to lift the 60/hour unauthenticated limit a busy runner IP hits."""
    seen = _sent(monkeypatch, token="t0ken")
    auth = [v for k, v in seen["headers"].items() if k.lower() == "authorization"]
    assert auth == ["Bearer t0ken"]
