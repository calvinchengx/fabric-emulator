"""Run Microsoft's `fab` CLI against the emulator, and read its answers safely.

THE SEAM IS DELIBERATE AND VISIBLE. `fab` can only reach hostnames that resolve
to the emulator on :443 with a trusted certificate, so it runs in a container on
the compose network (see docker-compose.yml). Every step below shells out
through `_compose_run`. Adapting this against real Fabric means deleting that
one function and calling `fab` directly — which is a diff you can see, rather
than a wrapper you have to unpick.

THE THING THIS FILE EXISTS TO PREVENT. `fab job run` exits **0 on a job that
never succeeded** — measured, not assumed: a notebook was allowed to time out,
`fab` cancelled it, and the process returned 0. Anything here that trusted
`check=True` would report success on every failed run, and the example would be
a row of green ticks proving nothing. So job outcomes are read back from
`fab job run-list --output_format json` and asserted on, and `test_fabctl.py`
feeds those assertions a cancelled run to prove they go red.

For contrast, `fab get` on a missing item DOES exit 1 — so the exit code is
worth checking everywhere except job outcomes, and `run()` checks it by default.
"""
from __future__ import annotations

import json
import pathlib
import re
import subprocess
import sys

HERE = pathlib.Path(__file__).resolve().parent
COMPOSE = ["docker", "compose", "-f", str(HERE / "docker-compose.yml")]

# The seeded capacity, from the emulator's default fixture data.
CAPACITY = "Emulator Capacity"

# `fab` addresses everything by a typed NAME path — `ws.Workspace/item.Notebook`
# — and resolves the GUIDs itself. That is why this example keeps no state file
# between steps, where the REST-driven examples all need one: the names below
# ARE the state, and each step can be run on its own.
WORKSPACE = "fabdriven.Workspace"
LAKEHOUSE = f"{WORKSPACE}/lake.Lakehouse"
BRONZE_PIPELINE = f"{WORKSPACE}/bronze-ingest.DataPipeline"
SILVER_NOTEBOOK = f"{WORKSPACE}/silver.Notebook"

# fab decorates output with ANSI colour even without a TTY. A colour escape
# inside a value is invisible in a terminal and fatal to json.loads, which is
# how the fabric-cli e2e driver ended up sanitising GUIDs by hand.
_ANSI = re.compile(r"\x1b\[[0-9;]*[a-zA-Z]")


def strip_ansi(text: str) -> str:
    return _ANSI.sub("", text).replace("\r", "")


def unwrap(payload):
    """Return the payload fab actually meant, whatever envelope it used.

    `fab --output_format json` wraps results differently per command — some
    return the value bare, some nest it under `data`/`result`/`text`/`body`, and
    some nest a JSON *string* one level down. Handling only the shape observed
    today would break on the next command added.

    `data` is first because it is the one `fab job run-list` actually uses, and
    leaving it out is how this example first failed at step 4: the job HAD
    completed, and check_completed refused it because the rows never came out of
    the envelope. That is the gate behaving correctly — it declined to guess.
    """
    for _ in range(4):  # bounded: a cycle here would hang the example
        if isinstance(payload, str):
            try:
                payload = json.loads(payload)
                continue
            except json.JSONDecodeError:
                return payload
        if isinstance(payload, dict):
            for key in ("data", "result", "text", "body", "value"):
                if key in payload and isinstance(payload[key], (dict, list, str)):
                    payload = payload[key]
                    break
            else:
                return payload
            continue
        return payload
    return payload


def as_rows(payload) -> list[dict]:
    """Normalise a fab result to a list of records.

    A single-item result comes back as an object, a listing as an array, and an
    empty listing as either `[]` or nothing at all. Callers want one shape.
    """
    payload = unwrap(payload)
    if payload is None:
        return []
    if isinstance(payload, dict):
        return [payload]
    if isinstance(payload, list):
        return [r for r in payload if isinstance(r, dict)]
    return []


# --- the container seam ----------------------------------------------------


def _compose_run(args: list[str]) -> subprocess.CompletedProcess:
    """One `fab` invocation, in a throwaway container on the compose network.

    `--no-deps` is not tidiness — it is load-bearing. `docker compose run` starts
    a service's dependencies, and RECREATES any whose resolved config differs
    from what is running. This stack keeps its store in memory, so a recreate
    destroys the workspace mid-run. It happened: a shell that resolved a
    different image tag than the one `up` had used turned step 4 into
    "The Workspace 'fabdriven.Workspace' could not be found" — the emulator had
    been silently replaced between two steps of the same pipeline.

    The fab container needs nothing started on its behalf anyway: entrypoint.sh
    waits for both hosts to answer TLS before it runs anything.
    """
    return subprocess.run(
        COMPOSE + ["run", "--rm", "--no-deps", "--quiet-pull", "fab", *args],
        cwd=HERE, capture_output=True, text=True,
    )


def run(*args: str, check: bool = True) -> str:
    """Run one `fab` command. Returns its stdout with ANSI stripped.

    `check` guards against a command that genuinely failed — a missing item, a
    malformed definition. It is NOT a guard against a failed job; see the module
    docstring, and use `job_run` for those.
    """
    proc = _compose_run(list(args))
    out = strip_ansi(proc.stdout)
    if check and proc.returncode != 0:
        sys.exit(
            f"fab {' '.join(args)} -> exit {proc.returncode}\n"
            f"{out}\n{strip_ansi(proc.stderr)}"
        )
    return out


def run_json(*args: str, check: bool = True):
    """Run a `fab` command in JSON mode and parse the result.

    `--output_format json` is not decoration: the alternative is scraping fab's
    aligned text tables, which change with terminal width and CLI version.
    """
    out = run(*args, "--output_format", "json", check=check)
    text = out.strip()
    if not text:
        return None
    # fab prints its own progress lines before the payload on some commands, so
    # take the JSON document rather than assuming the whole of stdout is one.
    start = min((i for i in (text.find("{"), text.find("[")) if i != -1), default=-1)
    if start == -1:
        return None
    try:
        return json.loads(text[start:])
    except json.JSONDecodeError:
        return None


def api_rows(path: str) -> list[dict]:
    """Rows from `fab api <path>` — which nests one level deeper than the rest.

    A raw passthrough hands back the HTTP response as well as its body, so the
    shape is:

        result -> data -> [{status_code, text: {value: [...]}}]

    Teaching the generic unwrap() to find that would mean letting it walk INTO
    lists, and a rule that general would cheerfully unwrap a one-element result
    set into its single row — turning a listing into a record with no signal.
    So this shape is handled where it is known, explicitly.

    The status code is checked here too. `fab api` exits 0 on a 4xx passthrough,
    the same way `fab job run` exits 0 on a failed job.
    """
    return api_rows_of(run_json("api", path), path)


def api_rows_of(payload, path: str = "") -> list[dict]:
    """The parsing half of api_rows, kept pure so it can be tested."""
    for key in ("result", "data"):
        if isinstance(payload, dict) and key in payload:
            payload = payload[key]
    if isinstance(payload, list) and payload and isinstance(payload[0], dict) \
            and "status_code" in payload[0]:
        response = payload[0]
        status = response.get("status_code")
        if status is None or not 200 <= int(status) < 300:
            raise AssertionError(f"fab api {path} -> HTTP {status}: {response}")
        payload = response.get("text")
    return as_rows(payload)


# --- jobs: where the exit code lies ---------------------------------------


def newest_run(runs: list[dict]) -> dict | None:
    """The most recently STARTED run, or None if there are none.

    Sorted rather than indexed: `fab job run-list` does not promise an order,
    and taking `runs[0]` would silently assert one.
    """
    dated = [r for r in runs if r.get("startTimeUtc")]
    if not dated:
        return runs[0] if len(runs) == 1 else None
    return max(dated, key=lambda r: r["startTimeUtc"])


def check_completed(runs: list[dict], what: str) -> dict:
    """Raise unless the newest run of `what` finished Completed.

    NO RUNS IS A FAILURE, not a pass. If `fab job run` never started anything,
    an emptiness check that returned quietly would be the most convincing green
    tick in the example and would mean nothing at all.
    """
    latest = newest_run(runs)
    if latest is None:
        raise AssertionError(
            f"{what}: fab reports NO job runs at all — the run never started, "
            f"which `fab job run` reported as success (exit 0)"
        )
    status = latest.get("status")
    if status != "Completed":
        raise AssertionError(
            f"{what}: job {latest.get('id')} ended {status!r}, not 'Completed' "
            f"(fab job run exited 0 regardless) — {json.dumps(latest)}"
        )
    return latest


def job_run(path: str, timeout: int = 300) -> dict:
    """Run an item to completion and PROVE it completed.

    Two commands, because one is not enough: `fab job run` blocks until the job
    reaches a terminal state, and `fab job run-list` is the only place that says
    which terminal state that was.
    """
    run("job", "run", path, "--timeout", str(timeout), check=False)
    runs = as_rows(run_json("job", "run-list", path))
    return check_completed(runs, path)


# --- small conveniences the steps share ------------------------------------


def item_id(path: str) -> str:
    """The GUID of a workspace or item, as fab reports it."""
    out = strip_ansi(run("get", path, "-q", "id"))
    m = re.search(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}", out)
    if not m:
        sys.exit(f"no id in `fab get {path} -q id` output: {out!r}")
    return m.group(0)


def exists(path: str) -> bool:
    return "true" in run("exists", path, check=False).lower()


def log(msg: str) -> None:
    print(f"    {msg}", flush=True)
