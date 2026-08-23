"""Framework-conformance probes: one contract, three backends.

`docs/38-framework-conformance.md` is the contract list. This file is the
assertion shape those contracts take when a backend is asked to prove them.

The first asserted row is contract 4 (write landing), because that is the
failure class that produced silent wrong answers rather than loud failures.
The rule that makes it real: **the engine that wrote must not be the one
that confirms.** Every false green in the document happened because success
was reported by the component doing the work.

Live I/O is injected. Without it, a required cell records a known gap with
a pointer rather than being skipped — the kit landed before every contract
passed, and a silent skip is how a gap becomes invisible again. Contract 4
is the first row a live backend replaces.
"""
from __future__ import annotations

from collections.abc import Callable
from dataclasses import asdict, dataclass

BACKENDS = ("sail", "jvm", "warehouse")

# Titles match the applicability table in docs/38, not the long section
# headings. The pointer is the heading slug GitHub (and the site) generate
# from those headings, so a cell opens the prose that closes the gap.
CONTRACTS: tuple[tuple[int, str, str], ...] = (
    (1, "Context chain",
     "38-framework-conformance.md#1-session-context-is-a-control-plane-contract-not-an-environment-variable"),
    (2, "Signature shape",
     "38-framework-conformance.md#2-the-api-shape-is-the-contract-independent-of-behaviour"),
    (3, "Runtime floor",
     "38-framework-conformance.md#3-the-runtime-is-a-versioned-product-not-some-spark"),
    (4, "Write landing",
     "38-framework-conformance.md#4-a-success-claim-must-be-witnessed-by-the-artifact"),
    (5, "Concurrent isolation",
     "38-framework-conformance.md#5-concurrency-is-the-default-case-not-the-edge-case"),
    (6, "Rewrite fall-through",
     "38-framework-conformance.md#6-engine-gaps-need-a-bounded-rewrite-escape-hatch-with-a-stated-contract"),
    (7, "Credential lifetime",
     "38-framework-conformance.md#7-credentials-must-outlive-the-run"),
)

# Mirrors the APPLICABILITY table. Duplicated here so a probe run does not
# have to parse markdown, and checked against the doc by the unit tests.
NOT_APPLICABLE = frozenset({
    (1, "warehouse"),
    (2, "warehouse"),
    (3, "warehouse"),
})
CONTROL = frozenset({(6, "jvm")})

# Why a cell is ❌ on the offline record() path (live_write is None).
# Contract 4 is replaced when a live backend records; the others wait on
# that same harness with different notebooks.
GAP_REASON = {
    1: "no live session recorded — the offline path cannot prove a session contract",
    2: "not yet asserted — needs a running notebook session",
    3: "not yet asserted — needs a running notebook session",
    4: "live backends not yet recorded",
    5: "not yet asserted — same harness as write landing, different notebooks",
    6: "not yet asserted — same harness as write landing, different notebooks",
    7: "not yet asserted — same harness as write landing, different notebooks",
}


@dataclass(frozen=True)
class WriteClaim:
    """What the emulator path reported after a write."""

    ok: bool
    error: str = ""


@dataclass(frozen=True)
class Artifact:
    """What an out-of-band reader saw. Must not be the writer."""

    found: bool
    location: str = ""


@dataclass(frozen=True)
class Result:
    id: str
    contract: str
    backend: str
    status: str  # pass | fail | gap | na
    error: str = ""
    pointer: str = ""

    def as_dict(self) -> dict:
        return asdict(self)


class SameReaderError(ValueError):
    """The writer was handed back as the reader.

    That is the exact shape of every false green in docs/38 §4: the engine's
    own catalog confirmed a write that never landed where Fabric puts it.
    """


def write_landing(
    *,
    writer: Callable[[], WriteClaim],
    reader: Callable[[], Artifact],
    expected_location: str,
    backend: str,
) -> Result:
    """Contract 4: a success claim is the artifact where Fabric would put it.

    `writer` is the emulator path (RunNotebook for a lakehouse, TDS for a
    warehouse). `reader` is out of band (delta-rs / OneLake DFS / a fresh
    TDS connection). Passing the same callable as both is a harness bug,
    not a failed probe — it is refused before any I/O runs.
    """
    title = CONTRACTS[3][1]
    pointer = CONTRACTS[3][2]
    if writer is reader:
        raise SameReaderError(
            "the engine that wrote must not be the one that confirms")

    claim = writer()
    artifact = reader()
    if claim.ok and artifact.found:
        return Result(id="4", contract=title, backend=backend, status="pass")
    if claim.ok and not artifact.found:
        where = expected_location or artifact.location or "the expected location"
        return Result(
            id="4", contract=title, backend=backend, status="fail",
            error=f"writer reported success; out-of-band reader found nothing at {where}",
            pointer=pointer,
        )
    return Result(
        id="4", contract=title, backend=backend, status="fail",
        error=claim.error or "writer reported failure",
        pointer=pointer,
    )


@dataclass(frozen=True)
class ContextClaim:
    """What one notebook session reported about its own identity.

    Each link of the chain is captured SEPARATELY, because a framework probes
    them in order and stops at the first that answers. A cell that reported
    only the union of them could pass while the link a framework actually
    reaches returns nothing.

    `env_fallback_set` records whether NOTEBOOKUTILS_WORKSPACE_ID /
    _LAKEHOUSE_ID were present in the session. If they were, this contract is
    unprovable on that run: the fallback can answer correctly while the two
    control-plane links are broken, which is the precise defect §1 describes.
    """

    ok: bool
    env_workspace: str = ""
    context_workspace: str = ""
    context_lakehouse: str = ""
    fallback_workspace: str = ""
    env_fallback_set: bool = False
    error: str = ""


def context_chain(
    *,
    session: Callable[[], ContextClaim],
    expected_workspace: str,
    expected_lakehouse: str,
    backend: str,
) -> Result:
    """Contract 1: every link answers with the RUNNING notebook's identity.

    THE COMPARISON IS AGAINST THE CONTROL PLANE, not against the session. A
    notebook is the only witness to its own context, so "the session said X"
    proves nothing on its own; what is checkable out of band is that X equals
    the ids the harness was issued when it created the workspace and lakehouse.
    The process doing the comparing never ran inside the session.

    A run with the environment fallback set is refused rather than passed. The
    fallback answering correctly is exactly how a broken `runtime.context`
    stayed invisible, so a green earned that way would re-create the defect.
    """
    title = CONTRACTS[0][1]
    pointer = CONTRACTS[0][2]
    claim = session()
    if not claim.ok:
        return Result(id="1", contract=title, backend=backend, status="fail",
                      error=claim.error or "the session reported no context",
                      pointer=pointer)
    if claim.env_fallback_set:
        return Result(
            id="1", contract=title, backend=backend, status="fail",
            error=("NOTEBOOKUTILS_WORKSPACE_ID/_LAKEHOUSE_ID were set in the "
                   "session, so the fallback could answer for the two "
                   "control-plane links — this run cannot prove the contract"),
            pointer=pointer)

    wrong = []
    for link, got, want in (
        ("env.getWorkspaceId()", claim.env_workspace, expected_workspace),
        ("runtime.context[currentWorkspaceId]", claim.context_workspace, expected_workspace),
        ("runtime.context[defaultLakehouseId]", claim.context_lakehouse, expected_lakehouse),
    ):
        if got != want:
            wrong.append(f"{link} = {got or '<empty>'}, control plane issued {want}")
    if wrong:
        return Result(id="1", contract=title, backend=backend, status="fail",
                      error="; ".join(wrong), pointer=pointer)
    return Result(id="1", contract=title, backend=backend, status="pass")


def record(backend: str, live: dict[int, Result] | None = None) -> list[dict]:
    """Results for one backend.

    `live` maps a contract number to the Result a live run produced. Anything
    absent from it records a gap with a pointer rather than being skipped —
    the kit landed before every contract passed, and a silent skip is how a
    gap becomes invisible again. A contract that is n/a on this backend stays
    n/a even if a live result is offered, because the applicability table is
    the authority on what a backend can be asked to prove.
    """
    if backend not in BACKENDS:
        raise ValueError(f"unknown backend {backend!r}; expected one of {BACKENDS}")
    live = live or {}
    unknown = set(live) - {num for num, _, _ in CONTRACTS}
    if unknown:
        raise ValueError(f"live results for unknown contract(s): {sorted(unknown)}")
    out: list[dict] = []
    for num, title, pointer in CONTRACTS:
        if (num, backend) in NOT_APPLICABLE:
            out.append(Result(
                id=str(num), contract=title, backend=backend, status="na",
            ).as_dict())
            continue
        if num in live:
            out.append(live[num].as_dict())
            continue
        out.append(Result(
            id=str(num), contract=title, backend=backend, status="gap",
            error=GAP_REASON[num], pointer=pointer,
        ).as_dict())
    return out
