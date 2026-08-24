"""The 200-or-202 outcome, once.

Fabric documents TWO outcomes for the same call — 200 with the body, or 202
with `Location` / `x-ms-operation-id` whose `/result` carries it — and a real
tenant answers 202. A client that reads the 202's body gets `null` and reports
an EMPTY result rather than an error, which is the quiet failure
`FABRIC_FORCE_LRO` exists to make reachable locally.

HERE BECAUSE IT WAS ABOUT TO BE WRITTEN A THIRD TIME. `notebook._follow` and
`variableLibrary._definition_parts` are the same loop with different exception
types, and `lakehouse.getDefinition` needed it next. Three copies of one rule
is the defect this repository keeps finding in other people's code.

The caller supplies its own error type, token audience AND request function.
The first two genuinely differ per module. The third is a lesson this
repository already paid for: `_config.session_workspace_id` called `config()`
from its own namespace, which bypassed every per-module test stub and 35 tests
said so at once. A helper that reaches for its own `request` is the same
mistake — the caller's module is where the stub is installed, so the caller
hands it over.
"""
import json
import time

from ._config import config
from ._http import request


def follow(status, headers, payload, *, what, token, send=request,
           error=RuntimeError, want_result=True, timeout=120):
    """Resolve the outcome. Returns the result document, or {}.

    `want_result` is False for operations with no result document — Fabric's
    updateDefinition is one, and polling `/result` for it 404s on success.
    """
    if status != 202:
        return json.loads(payload) if payload else {}
    op = headers.get("x-ms-operation-id") or headers.get("X-Ms-Operation-Id")
    location = headers.get("Location") or headers.get("location")
    if not op and not location:
        raise error(f"{what} returned 202 with no operation to follow")
    op_url = location or f"{config().fabric_url}/v1/operations/{op}"
    deadline = time.monotonic() + timeout
    while True:
        state = send("GET", op_url, token=token)
        st = state.get("status")
        if st == "Succeeded":
            break
        if st == "Failed":
            raise error(f"{what} operation failed: {state.get('error')}")
        if time.monotonic() > deadline:
            raise error(f"{what} operation did not complete")
        time.sleep(0.2)
    if not want_result:
        return {}
    return send("GET", op_url.rstrip("/") + "/result", token=token)
