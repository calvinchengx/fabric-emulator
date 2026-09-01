#!/usr/bin/env python3
"""The e2e MLflow server must not publish a port to the host.

WHY THIS EXISTS. GHSA-h7x2-h6g9-p789 (high): MLflow's AI Gateway stores an
`auth_config.api_base` verbatim with no validation of scheme, host or IP range,
and its proxy endpoint then fetches that address plus a caller-supplied path and
returns the body -- a server-side request forgery. MLflow's own SSRF guard,
`_validate_webhook_url`, is never reached on that path, and `CreateGatewaySecret`
has no entry in the permission-validator map, so basic authentication is enough.

THERE IS NO VERSION TO UPGRADE TO. The range is `>= 3.13.0, <= 3.15.2` and 3.15.2
is the newest release; GitHub records no patched version. Downgrading is not
available either: this project's floor is deliberately 3.15 because a resolve
below it removed `--disable-security-middleware`, the flag the e2e passes, and
the container then would not start (see the `mlflow` group in pyproject.toml).

SO THE MITIGATION IS REACHABILITY, and this asserts it rather than trusting it.
The server exists only inside an e2e compose network and publishes nothing: no
`ports:` key anywhere in that file. Nothing in this repository uses the AI
Gateway at all -- the emulator speaks only the tracking, registry and artifact
APIs -- but the endpoint still EXISTS on that container, so what keeps it
harmless is that nothing outside the compose network can reach it.

That is a property somebody could delete in one line while debugging, which is
exactly the kind of line that never gets reverted. Hence a check.

Usage:
    check_mlflow_unpublished.py      exit non-zero if the e2e stack publishes a port
"""
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
COMPOSE = ROOT / "e2e" / "data-science-loop" / "docker-compose.yml"


def main() -> int:
    if not COMPOSE.is_file():
        print(f"check_mlflow_unpublished: {COMPOSE} does not exist", file=sys.stderr)
        return 1
    text = COMPOSE.read_text(encoding="utf-8")

    offenders = [
        f"{n}: {line.strip()}"
        for n, line in enumerate(text.splitlines(), 1)
        if re.match(r"\s*ports:\s*$", line) or re.match(r"\s*ports:\s*\[", line)
    ]
    if offenders:
        print(
            "check_mlflow_unpublished: the data-science-loop stack publishes a "
            "port to the host:\n  " + "\n  ".join(offenders) + "\n\n"
            "MLflow's AI Gateway (GHSA-h7x2-h6g9-p789) has no fixed release and "
            "this stack runs a version inside the vulnerable range. Its only\n"
            "mitigation is that the server is reachable from the compose network "
            "and nowhere else. If a published port is genuinely needed, say so\n"
            "here and re-assess the advisory rather than deleting this check.",
            file=sys.stderr,
        )
        return 1

    print("check_mlflow_unpublished: the e2e MLflow server publishes no host port")
    return 0


if __name__ == "__main__":
    sys.exit(main())
