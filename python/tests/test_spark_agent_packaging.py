"""The published spark-agent image must CONTAIN the agent.

It did not, for every release up to and including 0.14.0. The image was built
from `docker/python-runtime/Dockerfile` — dependencies and nothing else — and
the agent reached the container through a `./e2e/livy:/livy` bind mount that
every compose file in this repository set. Inside the project the stack worked;
from outside it, `ghcr.io/calvinchengx/fabric-emulator-spark-agent:0.14.0` was
a Python runtime with no agent in it:

    docker run --rm --entrypoint sh …/fabric-emulator-spark-agent:0.14.0 \\
      -c "ls /livy"        ->  no such directory

Nothing failed loudly. A consumer started the service, the emulator was handed
`FABRIC_SPARK_AGENT_URL`, and RunNotebook jobs simply never reached a terminal
state — which since 0.14.0 surfaces as `NotebookError` on timeout, an error
naming neither the image nor the missing file.

These tests are cheap and static on purpose: they run in the same suite as
everything else rather than behind a Docker build, because the failure they
guard is a packaging omission, and an omission is exactly what a test that
needs infrastructure to run tends not to catch.
"""

import pathlib
import re

import pytest
import yaml

ROOT = pathlib.Path(__file__).resolve().parents[2]
AGENT_DOCKERFILE = ROOT / "docker" / "spark-agent" / "Dockerfile"
RUNTIME_DOCKERFILE = ROOT / "docker" / "python-runtime" / "Dockerfile"


class _ComposeLoader(yaml.SafeLoader):
    """SafeLoader that tolerates Compose's merge tags.

    `!override` and `!reset` are Compose's own, and SafeLoader refuses an
    unknown tag outright — so reading docker-compose.spark-jvm.yml raises
    rather than returning a document. Dropping the tag and keeping the value is
    right for these tests, which ask what a service mounts, not how Compose
    merges it.
    """


_ComposeLoader.add_multi_constructor(
    "!", lambda loader, suffix, node: _ComposeLoader.construct_scalar(loader, node)
    if isinstance(node, yaml.ScalarNode)
    else (
        _ComposeLoader.construct_sequence(loader, node)
        if isinstance(node, yaml.SequenceNode)
        else _ComposeLoader.construct_mapping(loader, node)
    ),
)


def _compose(path: pathlib.Path) -> dict:
    return yaml.load(path.read_text(), Loader=_ComposeLoader) or {}


def _directives(dockerfile: pathlib.Path, verb: str) -> list[str]:
    """Every argument of one Dockerfile instruction, comments stripped."""
    out = []
    for line in dockerfile.read_text().splitlines():
        line = line.strip()
        if line.startswith("#"):
            continue
        m = re.match(rf"^{verb}\s+(.*)$", line, re.I)
        if m:
            out.append(m.group(1).strip())
    return out


def test_the_spark_agent_ships_inside_its_image():
    """The Dockerfile must copy the agent in and default to running it.

    Both halves matter. Copying without a CMD leaves the consumer to guess the
    path, and a CMD naming a file the image never copied is the original bug
    with an extra step.
    """
    copies = _directives(AGENT_DOCKERFILE, "COPY")
    assert any(c.startswith("python/") for c in copies), (
        f"{AGENT_DOCKERFILE.relative_to(ROOT)} never copies python/, so the "
        f"agent is not in the image: {copies}"
    )

    cmds = _directives(AGENT_DOCKERFILE, "CMD")
    assert len(cmds) == 1, f"expected exactly one CMD, got {cmds}"
    # The CMD is JSON-array form; pull out the path it runs.
    paths = re.findall(r'"(/[^"]+\.py)"', cmds[0])
    assert len(paths) == 1, f"CMD names no single script: {cmds[0]}"

    # /app is the WORKDIR the COPY lands under, so the container path maps back
    # onto a real file in this repository. A CMD pointing at a script nobody
    # wrote fails only at `docker run`, in someone else's CI.
    on_disk = ROOT / paths[0].removeprefix("/app/")
    assert on_disk.is_file(), f"CMD runs {paths[0]}, which is not in the repo"


def test_the_agent_is_not_bind_mounted_over_its_own_image():
    """A mount would hide a repeat of the packaging bug.

    This is the check that would have caught it the first time. Every local
    compose file mounted the source tree over the agent, so the image being
    empty changed nothing anyone could observe here — the omission was visible
    only to someone with no clone, which is to say only to a user.
    """
    offenders = []
    for compose in sorted(ROOT.glob("docker-compose*.yml")):
        doc = _compose(compose)
        for name, svc in (doc.get("services") or {}).items():
            build = svc.get("build") or {}
            dockerfile = build.get("dockerfile") if isinstance(build, dict) else None
            if dockerfile != "docker/spark-agent/Dockerfile":
                continue
            for vol in svc.get("volumes") or []:
                target = vol.split(":")[1] if isinstance(vol, str) and ":" in vol else ""
                if "spark_agent" in str(vol) or target.rstrip("/") in ("/livy", "/app/python"):
                    offenders.append(f"{compose.name}:{name}: {vol}")
    assert not offenders, (
        "these mounts complete the image instead of configuring it, which is "
        f"how the empty image went unnoticed: {offenders}"
    )


def test_the_runtime_images_share_one_preamble():
    """Two Dockerfiles now install the same environment; they must agree.

    The duplication is deliberate — factoring it into a base image would make
    the agent's build wait on a push of that base, which is a release-ordering
    constraint in exchange for six lines. The cost of the choice is drift, so
    the interpreter, the uv release and the venv path are pinned together here
    rather than left to whoever edits one file and not the other.
    """
    # WHAT IS SHARED IS THE ENVIRONMENT, not what the image claims to BE.
    # `FABRIC_RUNTIME` is a declaration that the image is a Fabric notebook
    # runtime and meets that runtime's floor (docs/38 §3). The agent executes
    # notebook cells and can say that; python-runtime is a client image that
    # never runs a cell, and declaring it there would be a false claim — the
    # opposite of what this file is for. So it is compared out by name rather
    # than by loosening the check.
    image_specific = {"FABRIC_RUNTIME"}

    def shared(path, verb):
        return [d for d in _directives(path, verb)
                if d.split("=", 1)[0] not in image_specific]

    for verb in ("FROM", "WORKDIR", "ENV"):
        agent = shared(AGENT_DOCKERFILE, verb)
        runtime = shared(RUNTIME_DOCKERFILE, verb)
        assert agent == runtime, (
            f"{verb} drifted between the two runtime images: "
            f"spark-agent={agent} python-runtime={runtime}"
        )

    # The declaration itself is still pinned — just to the images that execute
    # notebook code, which is the claim it makes.
    assert "ENV FABRIC_RUNTIME=" in AGENT_DOCKERFILE.read_text(encoding="utf-8")
    assert "ENV FABRIC_RUNTIME=" not in RUNTIME_DOCKERFILE.read_text(encoding="utf-8"), (
        "python-runtime is a client image; declaring a Fabric Runtime there "
        "would claim it runs notebook cells")

    # COPY differs by one line (the agent needs no build ARG), so compare the
    # lines that carry the dependency set rather than the whole list.
    for shared in ("pyproject.toml uv.lock ./", "python/ ./python/"):
        assert shared in _directives(AGENT_DOCKERFILE, "COPY")
        assert shared in _directives(RUNTIME_DOCKERFILE, "COPY")


def test_the_release_builds_the_agent_from_its_own_dockerfile():
    """The published tag is what a consumer pulls; the fix has to reach it.

    Repairing the Dockerfile while the release matrix still points at the
    shared runtime would fix the repository and ship the same broken image.
    """
    wf = yaml.safe_load((ROOT / ".github" / "workflows" / "release.yml").read_text())
    images = [
        job
        for job in wf["jobs"].values()
        if isinstance(job, dict) and "strategy" in job
        for job in job["strategy"]["matrix"].get("include", [])
    ]
    agent = [i for i in images if i.get("name") == "spark-agent"]
    assert len(agent) == 1, f"expected one spark-agent image build, got {agent}"
    assert agent[0]["dockerfile"] == "docker/spark-agent/Dockerfile"


@pytest.mark.parametrize("module", ["agent.py", "delta_ops.py", "storage.py", "catalog.py", "eventstream_kafka.py", "jvmconf.py"])
def test_the_agent_sources_live_under_python(module: str):
    """`python/` is what the image copies, so location IS packaging here.

    These files sat in `e2e/` and were therefore excluded by construction. The
    directory they live in is the fix, not the Dockerfile line that copies it.
    """
    assert (ROOT / "python" / "spark_agent" / module).is_file()
    assert not (ROOT / "e2e" / "livy" / module).exists(), (
        f"{module} is back under e2e/, where the published image cannot see it"
    )
