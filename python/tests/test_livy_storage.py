"""Pin the env -> delta-rs storage_options mapping used by the Spark agent.

These key names are not free choices: they are what `object_store` accepts, and
a typo produces no error at all — unknown keys are accepted silently, so a
misspelled `azure_storage_token` degrades to *anonymous access* rather than
failing. That is exactly the failure the OneLake witness in `e2e/livy` catches
with its negative control, but this test catches it in milliseconds.
"""
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "spark_agent"))

import storage


@pytest.fixture(autouse=True)
def _clear_cache():
    storage._cached_token = None
    storage._cached_good_until = 0.0


def test_no_account_yields_no_options():
    # The local-path case. Half-populated options fail more confusingly than
    # none, and delta-rs has its own env handling to fall back on.
    assert storage.options({}) == {}
    assert storage.options({"AZURE_STORAGE_TOKEN": "t"}) == {}


def test_full_environment_maps_to_object_store_keys():
    got = storage.options({
        "AZURE_STORAGE_ACCOUNT_NAME": "onelake",
        "AZURE_STORAGE_ENDPOINT": "https://fabric-emulator:9443/onelake",
        "AZURE_STORAGE_TOKEN": "bearer-abc",
        "AZURE_ALLOW_HTTP": "true",
        "AZURE_ALLOW_INVALID_CERTIFICATES": "true",
    })
    assert got == {
        "azure_storage_account_name": "onelake",
        "azure_endpoint": "https://fabric-emulator:9443/onelake",
        "azure_storage_token": "bearer-abc",
        "azure_allow_http": "true",
        "azure_allow_invalid_certificates": "true",
    }


def test_optional_keys_are_omitted_not_blanked():
    # An empty azure_allow_http is not the same as an absent one: object_store
    # parses the value, so "" would be a parse error rather than a default.
    got = storage.options({"AZURE_STORAGE_ACCOUNT_NAME": "onelake"})
    assert got == {"azure_storage_account_name": "onelake"}


def test_static_token_wins_over_minting(monkeypatch):
    # How the image is pointed at real Azure, or at a fixed test token, with no
    # issuer to call. If minting ran anyway this would raise.
    monkeypatch.setattr(storage, "_mint", lambda url, env: pytest.fail("minted"))
    env = {"AZURE_STORAGE_TOKEN": "static", "ENTRA_TOKEN_URL": "https://issuer"}
    assert storage.token(env) == "static"


def test_token_is_cached_until_it_nears_expiry(monkeypatch):
    calls = []

    def fake_mint(url, env):
        calls.append(url)
        return f"token-{len(calls)}", 3600.0

    monkeypatch.setattr(storage, "_mint", fake_mint)
    env = {"ENTRA_TOKEN_URL": "https://issuer", "ENTRA_CLIENT_ID": "c",
           "ENTRA_CLIENT_SECRET": "s"}
    assert storage.token(env) == "token-1"
    assert storage.token(env) == "token-1", "second call should hit the cache"
    assert calls == ["https://issuer"]

    # Past the refresh point, a new grant is made — the reason install() takes
    # the resolver as a callable rather than a dict.
    storage._cached_good_until = 0.0
    assert storage.token(env) == "token-2"


def test_short_lifetime_still_caches_for_a_minute(monkeypatch):
    # A skew-adjusted lifetime would be negative here; minting per statement
    # would turn a REPL into a token flood.
    monkeypatch.setattr(storage, "_mint", lambda url, env: ("t", 30.0))
    env = {"ENTRA_TOKEN_URL": "https://issuer", "ENTRA_CLIENT_ID": "c",
           "ENTRA_CLIENT_SECRET": "s"}
    storage.token(env)
    assert storage._cached_good_until > 0
