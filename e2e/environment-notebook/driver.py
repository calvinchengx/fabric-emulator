"""An Environment bound by a NOTEBOOK supplies a package the image lacks.

WHY THIS EXISTS SEPARATELY FROM e2e/environment. There are two ways to bind an
Environment. A Livy session names one by id at create; a **notebook** names one
in its own `# META` dependencies, and that is how most Fabric data engineering
does it. They are different code paths, and only the first ever worked:
`resolveComputeBinding` resolved the Environment onto a notebook run and the run
detail reported it, while the notebook driver never handed it to the agent. So a
notebook's first import failed with `ModuleNotFoundError` while the metadata said
the dependency had been honoured — resolved, reported, ignored, which is worse
than unimplemented because it reads as done.

WHY IT NEEDS ITS OWN STACK. The spark agent is ONE long-lived process, so a
package installed by any suite is importable by every session after it. Written
first inside e2e/environment, this suite's negative half passed on a package the
Livy half had already installed and proved nothing at all. An agent that has
installed nothing is the only thing that makes "without an Environment it fails"
evidence.

The shape is the same as its sibling, deliberately: assert that a **notebook
cell imports a module that was not in the image**, and assert first that the
same notebook FAILS without the binding.
"""

import base64
import json
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

ENTRA = "http://entra-emulator:8443"
FABRIC = "http://fabric-emulator"
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"
CLIENT = "00d88624-f0d7-46f6-a641-6232c2608928"
SECRET = "daemon-app-secret"

# Small, pure-Python, no dependencies, and not in the agent image. The import is
# the assertion, so the package only has to be absent and cheap.
PACKAGE = "cowsay"


def fail(msg):
    print(f"FAIL: {msg}", flush=True)
    sys.exit(1)


def http(method, url, body=None, token=None, form=False):
    headers, data = {}, None
    if body is not None:
        if form:
            data = urllib.parse.urlencode(body).encode()
            headers["Content-Type"] = "application/x-www-form-urlencoded"
        else:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
    if token:
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    with urllib.request.urlopen(req) as resp:
        raw = resp.read()
        if not raw:
            return resp.status, None
        try:
            return resp.status, json.loads(raw)
        except json.JSONDecodeError:
            return resp.status, None


def resolve(wsid, name, kind, token):
    """An item's id, by name. A definition-bearing create is a 202 with no body."""
    for _ in range(120):
        _, items = http("GET", f"{FABRIC}/v1/workspaces/{wsid}/items", token=token)
        for it in (items or {}).get("value", []):
            if it.get("displayName") == name and it.get("type") == kind:
                return it["id"]
        time.sleep(0.5)
    fail(f"the {kind} {name} never appeared")


def run_notebook(wsid, name, meta, token):
    """Publish a notebook that imports PACKAGE, run it, return the job instance."""
    src = (
        "# Fabric notebook source\n\n"
        "# CELL ********************\n\n"
        f"import {PACKAGE}\n"
        f"notebookutils.notebook.exit({PACKAGE}.__name__)\n"
    ) + meta
    payload = base64.b64encode(src.encode()).decode()
    http(
        "POST",
        f"{FABRIC}/v1/workspaces/{wsid}/items",
        {
            "displayName": name,
            "type": "Notebook",
            "definition": {
                "parts": [
                    {
                        "path": "notebook-content.py",
                        "payloadType": "InlineBase64",
                        "payload": payload,
                    }
                ]
            },
        },
        token=token,
    )
    nb = resolve(wsid, name, "Notebook", token)

    http(
        "POST",
        f"{FABRIC}/v1/workspaces/{wsid}/items/{nb}/jobs/instances?jobType=RunNotebook",
        {},
        token=token,
    )
    for _ in range(240):
        _, jobs = http(
            "GET",
            f"{FABRIC}/v1/workspaces/{wsid}/items/{nb}/jobs/instances",
            token=token,
        )
        runs = (jobs or {}).get("value", [])
        if runs and runs[0].get("status") in ("Completed", "Failed", "Cancelled"):
            return runs[0]
        time.sleep(1)
    fail(f"the notebook run for {name} never reached a terminal state")


def main():
    _, tok = http(
        "POST",
        f"{ENTRA}/{TENANT}/oauth2/v2.0/token",
        {
            "grant_type": "client_credentials",
            "client_id": CLIENT,
            "client_secret": SECRET,
            "scope": "https://api.fabric.microsoft.com/.default",
        },
        form=True,
    )
    token = tok["access_token"]

    _, ws = http("POST", f"{FABRIC}/v1/workspaces", {"displayName": "env-nb-ws"}, token=token)
    wsid = ws["id"]

    payload = base64.b64encode(f"{PACKAGE}\n".encode()).decode()
    status, _ = http(
        "POST",
        f"{FABRIC}/v1/workspaces/{wsid}/items",
        {
            "displayName": "team-env",
            "type": "Environment",
            "definition": {
                "parts": [
                    {
                        "path": "Libraries/requirements.txt",
                        "payloadType": "InlineBase64",
                        "payload": payload,
                    }
                ]
            },
        },
        token=token,
    )
    if status != 202:
        fail(f"creating the Environment answered {status}, want 202")
    env_id = resolve(wsid, "team-env", "Environment", token)

    # 1. WITHOUT the binding, the import must fail. This half is what makes the
    #    positive one evidence rather than coincidence, and it is why this suite
    #    has an agent to itself.
    # RETRIED UNTIL THE AGENT IS UP, and then asserted BY REASON.
    #
    # The agent starts slower than the control plane, and a notebook submitted
    # before it listens fails with "the Spark agent is unreachable". That is a
    # Failed run, so a test asserting only `status == "Failed"` goes green on a
    # stack where nothing was ever executed — this suite did exactly that on its
    # first run, and the negative half proved nothing. So the reason has to say
    # the import is what failed.
    plain = None
    for _ in range(60):
        plain = run_notebook(wsid, f"env-nb-plain-{_}", "", token)
        why = json.dumps(plain.get("failureReason") or {})
        if "unreachable" in why or "connection refused" in why:
            time.sleep(5)
            continue
        break
    if plain.get("status") != "Failed":
        fail(
            f"a notebook with NO Environment imported {PACKAGE} "
            f"({plain.get('status')}) — the package is already reachable, so "
            f"nothing here would prove the binding did anything"
        )
    why = json.dumps(plain.get("failureReason") or {})
    if "ModuleNotFoundError" not in why:
        fail(
            f"the notebook failed, but not on the import: {why[:400]}. A run that "
            f"fails for any other reason would make the positive half below look "
            f"like evidence when it is not"
        )
    print(
        f"OK: without an Environment, a notebook fails on `import {PACKAGE}` "
        f"with ModuleNotFoundError",
        flush=True,
    )

    # 2. WITH the Environment named in its META, the same notebook must succeed.
    meta = (
        "\n# METADATA ********************\n\n"
        "# META {\n"
        '# META   "dependencies": {\n'
        '# META     "environment": {\n'
        f'# META       "environmentId": "{env_id}",\n'
        f'# META       "workspaceId": "{wsid}"\n'
        "# META     }\n"
        "# META   }\n"
        "# META }\n"
    )
    got = run_notebook(wsid, "env-nb-bound", meta, token)
    if got.get("status") != "Completed":
        fail(f"a notebook binding the Environment still failed: {json.dumps(got)[:400]}")
    print(
        f"OK: `import {PACKAGE}` succeeds in a NOTEBOOK only when its META names "
        f"the Environment — the binding is applied, not merely resolved",
        flush=True,
    )


try:
    main()
except urllib.error.HTTPError as err:  # report the body, not just the code
    fail(f"{err.code} {err.reason}: {err.read()[:400]!r}")
