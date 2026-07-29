"""The env emitter — the toggle for tools that only read environment.

    eval "$(python -m fabric_target env emulator)"
    eval "$(python -m fabric_target env real)"
    python -m fabric_target show

Emits the coherent variable set for the chosen target: notebookutils shim
wiring, azcopy/dbt auth hints, and the FABRIC_TARGET marker itself. Real mode
emits NO seeded values — credentials come from az login or AZURE_* vars the
user already controls.
"""
import shlex
import sys

from . import (SEED_CLIENT_ID, SEED_CLIENT_SECRET, Target, TargetError, _env)


def _lines(t):
    e = [("FABRIC_TARGET", t.name)]
    if t.is_emulator:
        e += [
            ("NOTEBOOKUTILS_FABRIC_URL", t.api_root.removesuffix("/v1")),
            ("NOTEBOOKUTILS_ENTRA_URL", t.entra_url),
            ("NOTEBOOKUTILS_TENANT", t.tenant),
            ("NOTEBOOKUTILS_CLIENT_ID", _env("FABRIC_CLIENT_ID", SEED_CLIENT_ID)),
            ("NOTEBOOKUTILS_CLIENT_SECRET", _env("FABRIC_CLIENT_SECRET", SEED_CLIENT_SECRET)),
            ("NOTEBOOKUTILS_VAULT_URL", t.vault_url),
            ("NOTEBOOKUTILS_INSECURE", "1"),
        ]
    else:
        e += [
            ("NOTEBOOKUTILS_FABRIC_URL", "https://api.fabric.microsoft.com"),
            ("NOTEBOOKUTILS_ENTRA_URL", t.entra_url),
            ("NOTEBOOKUTILS_TENANT", t.tenant),
            ("NOTEBOOKUTILS_INSECURE", "0"),
            # az-login-friendly auth hints for env-driven tools:
            ("AZCOPY_AUTO_LOGIN_TYPE", "AZCLI"),
        ]
        if t.vault_url:
            e.append(("NOTEBOOKUTILS_VAULT_URL", t.vault_url))
    return e


def main(argv):
    cmd = argv[1] if len(argv) > 1 else "show"
    name = argv[2] if len(argv) > 2 else None
    try:
        t = Target(name) if name else Target(_env("FABRIC_TARGET", "emulator"))
    except TargetError as err:
        print(f"# fabric_target: {err}", file=sys.stderr)
        return 1

    if cmd == "env":
        for k, v in _lines(t):
            print(f"export {k}={shlex.quote(v)}")
        # Unexported on purpose (comments are eval-safe):
        print(f"# fabric_target: profile '{t.name}' — api {t.api_root}")
        if t.is_real:
            print("# credentials: az login (or AZURE_* env vars, which take precedence)")
        return 0
    if cmd == "show":
        for k, v in [("target", t.name), ("api_root", t.api_root),
                     ("entra", t.entra_url), ("tenant", t.tenant),
                     ("onelake", t.onelake_url), ("vault", t.vault_url or "—"),
                     ("tls_verify", str(t.tls_verify)),
                     ("workspace_scope", t.workspace_scope or "—")]:
            print(f"{k:16} {v}")
        return 0
    print(f"usage: python -m fabric_target [env|show] [emulator|real]", file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv))
