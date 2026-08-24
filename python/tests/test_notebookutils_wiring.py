"""Every image that runs notebook cells can reach Fabric and Entra.

WHY THIS FILE EXISTS. `notebookutils` defaults to `http://127.0.0.1:19080`,
which inside a container is nothing. No compose in this repository gave the
spark-agent `NOTEBOOKUTILS_FABRIC_URL`, so the shim worked in a Jupyter cell —
that service has carried the wiring all along — and failed inside a
`RunNotebook` cell with `Connection refused`, while `docker-compose.yml`'s own
comment on the jupyter service claims "a cell here and a cell in a RunNotebook
job execute identically".

Nothing caught it because nothing had ever called notebookutils from inside a
notebook job. Framework-conformance contract 1 (docs/38 §1) did, on its first
run. This file is the cheap guard so the next compose cannot arrive without it.
"""
import pathlib
import re

import pytest
import yaml

ROOT = pathlib.Path(__file__).resolve().parents[2]

# The endpoints the shim resolves against. `_INSECURE` is here because the
# emulator serves them over plain HTTP on non-DNS hosts.
REQUIRED = ("NOTEBOOKUTILS_FABRIC_URL", "NOTEBOOKUTILS_ENTRA_URL")

# The environment FALLBACK, which must never be set for an agent: a runtime that
# answers `getWorkspaceId()` out of the environment can hide two broken
# control-plane links, which is exactly the defect docs/38 §1 describes. The
# agent binds the real context per statement.
FORBIDDEN = ("NOTEBOOKUTILS_WORKSPACE_ID", "NOTEBOOKUTILS_LAKEHOUSE_ID")


class _ComposeLoader(yaml.SafeLoader):
    """Compose's own YAML tags are not YAML the safe loader knows.

    `image: !reset null` and `depends_on: !override` are merge directives for
    `docker compose -f a -f b`. A safe loader raises on them, so a test that
    used one would skip exactly the overlay files most likely to drift — the
    ones that redefine a service. Unknown tags resolve to their underlying
    value, which is all this file reads.
    """


_ComposeLoader.add_multi_constructor(
    "", lambda loader, suffix, node: (
        loader.construct_mapping(node) if isinstance(node, yaml.MappingNode)
        else loader.construct_sequence(node) if isinstance(node, yaml.SequenceNode)
        else loader.construct_scalar(node)))


def _load(path):
    return yaml.load(path.read_text(encoding="utf-8"), Loader=_ComposeLoader)


def _composes_defining_spark_agent():
    seen = []
    for path in sorted(ROOT.glob("docker-compose*.yml")):
        text = path.read_text(encoding="utf-8")
        if re.search(r"^  spark-agent:\s*$", text, re.MULTILINE):
            seen.append(path)
    return seen


def test_there_is_something_to_check():
    """A scan that found nothing would pass for the wrong reason."""
    assert _composes_defining_spark_agent()


@pytest.mark.parametrize("path", _composes_defining_spark_agent(),
                         ids=lambda p: p.name)
def test_a_shipped_agent_can_reach_fabric_and_entra(path):
    env = _load(path)["services"]["spark-agent"]
    env = env.get("environment") or {}
    for key in REQUIRED:
        assert key in env, (
            f"{path.name}: spark-agent has no {key}, so notebookutils falls back "
            f"to 127.0.0.1:19080 and every call from a RunNotebook cell fails "
            f"with Connection refused")


@pytest.mark.parametrize("path", _composes_defining_spark_agent(),
                         ids=lambda p: p.name)
def test_a_shipped_agent_does_not_set_the_context_fallback(path):
    env = _load(path)["services"]["spark-agent"]
    env = env.get("environment") or {}
    for key in FORBIDDEN:
        assert key not in env, (
            f"{path.name}: spark-agent sets {key}. The environment fallback can "
            f"answer correctly while `runtime.context` is broken, which is how "
            f"that defect stayed invisible (docs/38 §1)")
