"""The real-Fabric gate must SKIP the conformance job, never fail the run.

`release.yml` makes every publishing job `needs: fabric-qualification`, so what
this gate does when the tenant secrets are absent decides whether a tag can
ship. It has been both ways: it failed closed, which blocked v0.35.0 on a
repository whose secrets have never been set, and before that it skipped. The
behaviour is therefore worth asserting rather than reading, in both directions.

THE GATE'S OWN SCRIPT IS EXECUTED, not pattern-matched. The step is shell, and
a shell step can be wrong in ways a regex over its source cannot see: an
unquoted expansion, a `set -e` interaction, a write to the wrong file. These
run it under the runner's exact shell flags -- GitHub invokes `shell: bash` as
`bash --noprofile --norc -e -o pipefail`, so -e is on before the script's own
`set` line and a bare failing assignment would end the step early.
"""
import os
import pathlib
import shutil
import subprocess

import pytest
import yaml

REPO = pathlib.Path(__file__).resolve().parents[2]
WORKFLOW = REPO / ".github" / "workflows" / "real-fabric.yml"


def _gate_script() -> str:
    d = yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))
    steps = d["jobs"]["gate"]["steps"]
    for s in steps:
        if s.get("id") == "check":
            return s["run"]
    raise AssertionError("the gate has no step with id 'check'")


def _working_bash():
    """A bash that actually executes a script, or None.

    NOT `shutil.which("bash")` alone. On windows-latest that resolves to
    System32\\bash.exe, the WSL launcher, which has no installed distribution
    and answers every invocation with "Windows Subsystem for Linux has no
    installed distributions" on stdout and a non-zero status. Every assertion
    in this file then failed against that banner rather than against the gate.
    Probing beats trusting the name: whatever is found has to run a script
    before it is used.

    Git Bash is tried first by absolute path because it is the interpreter
    Windows runners actually have and `which` does not find it first.
    """
    candidates = [
        r"C:\Program Files\Git\bin\bash.exe",
        r"C:\Program Files (x86)\Git\bin\bash.exe",
    ]
    found = shutil.which("bash")
    if found:
        candidates.append(found)
    for c in candidates:
        if not pathlib.Path(c).exists():
            continue
        try:
            r = subprocess.run([c, "-c", "printf ok"], capture_output=True, text=True, timeout=30)
        except OSError:
            continue
        if r.returncode == 0 and r.stdout.strip() == "ok":
            return c
    return None


def _run(tmp_path, *, client_id, tenant_id, workspace, client_secret=False):
    # THE ENVIRONMENT IS INHERITED, not pinned to a minimal one. The first
    # version passed `PATH=/usr/bin:/bin`, which is a POSIX path list, so the
    # whole file died on windows-latest where the suite also runs. The four
    # HAVE_* and the two GITHUB_* below are overridden explicitly, which is
    # what the isolation actually needs: a real GITHUB_OUTPUT in CI must not
    # be written to, and a stray HAVE_* must not leak in.
    bash = _working_bash()
    if bash is None:
        pytest.skip("no usable bash; the gate is a bash step and runs on ubuntu-latest")
    script = tmp_path / "gate.sh"
    script.write_text(_gate_script(), encoding="utf-8")
    out, summary = tmp_path / "out", tmp_path / "summary"
    out.touch()
    summary.touch()
    r = subprocess.run(
        [bash, "--noprofile", "--norc", "-e", "-o", "pipefail", str(script)],
        capture_output=True, text=True,
        env={
            **os.environ,
            "HAVE_CLIENT_ID": str(client_id).lower(),
            "HAVE_TENANT_ID": str(tenant_id).lower(),
            "HAVE_WORKSPACE": str(workspace).lower(),
            "HAVE_CLIENT_SECRET": str(client_secret).lower(),
            "GITHUB_OUTPUT": str(out),
            "GITHUB_STEP_SUMMARY": str(summary),
        },
    )
    outputs = dict(
        line.split("=", 1)
        for line in out.read_text(encoding="utf-8").splitlines()
        if "=" in line
    )
    return r, outputs, summary.read_text(encoding="utf-8")


def test_no_secrets_skips_and_does_not_fail(tmp_path):
    """The state this repository is actually in, and the one that blocked a tag."""
    r, outputs, _ = _run(tmp_path, client_id=False, tenant_id=False, workspace=False)
    assert r.returncode == 0, f"the gate failed the run instead of skipping:\n{r.stdout}{r.stderr}"
    assert outputs["run"] == "false"


def test_a_skip_is_loud(tmp_path):
    """The warning and the summary are the ONLY remaining record that a release
    from this run was never compared against Fabric. Silence here is the whole
    risk of skipping."""
    r, _, summary = _run(tmp_path, client_id=False, tenant_id=False, workspace=False)
    assert "::warning::" in r.stdout
    assert "AZURE_CLIENT_ID" in r.stdout and "FABRIC_TEST_WORKSPACE" in r.stdout
    assert "Nothing here was compared against Microsoft Fabric" in summary


@pytest.mark.parametrize(
    "client_id,tenant_id,workspace",
    [(True, True, False), (True, False, True), (False, True, True), (True, False, False)],
)
def test_every_partial_configuration_skips_and_says_so(tmp_path, client_id, tenant_id, workspace):
    """Half-configured is not the same situation as unconfigured -- it is nearly
    always a typo or a half-finished rotation -- and it must not read as the
    untouched case. It still skips; it just says which it is."""
    r, outputs, _ = _run(tmp_path, client_id=client_id, tenant_id=tenant_id, workspace=workspace)
    assert r.returncode == 0
    assert outputs["run"] == "false"
    assert "PARTIALLY configured" in r.stdout


def test_a_complete_configuration_runs(tmp_path):
    """The gate must not skip everything unconditionally -- which is the way a
    skip-by-default gate silently stops being a gate at all."""
    r, outputs, _ = _run(tmp_path, client_id=True, tenant_id=True, workspace=True)
    assert r.returncode == 0
    assert outputs["run"] == "true"
    assert outputs["mode"] == "oidc", "no client secret means OIDC"


def test_a_client_secret_selects_secret_mode(tmp_path):
    _, outputs, _ = _run(tmp_path, client_id=True, tenant_id=True, workspace=True, client_secret=True)
    assert outputs["run"] == "true"
    assert outputs["mode"] == "secret"


def test_the_conformance_job_is_gated_on_the_output_this_sets():
    """The tests above prove the gate emits `run`. This proves the emission is
    connected to something: without this, `run=false` could be correct and the
    conformance job still execute."""
    d = yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))
    assert d["jobs"]["conformance"]["if"] == "needs.gate.outputs.run == 'true'"
    assert d["jobs"]["gate"]["outputs"]["run"] == "${{ steps.check.outputs.run }}"
