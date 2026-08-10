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

from . import Target, TargetError, _env, notebook_env


def main(argv):
    cmd = argv[1] if len(argv) > 1 else "show"
    name = argv[2] if len(argv) > 2 else None
    try:
        t = Target(name) if name else Target(_env("FABRIC_TARGET", "emulator"))
    except TargetError as err:
        print(f"# fabric_target: {err}", file=sys.stderr)
        return 1

    if cmd == "env":
        for k, v in notebook_env(t):
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
    print("usage: python -m fabric_target [env|show] [emulator|real]", file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv))
