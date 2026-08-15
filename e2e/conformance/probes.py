"""Framework-conformance probes: one contract, three backends.

`docs/38-framework-conformance.md` is the contract list. This file is the
assertion shape those contracts take when a backend is asked to prove them.

The first asserted row is contract 4 (write landing), because that is the
failure class that produced silent wrong answers rather than loud failures.
The rule that makes it real: **the engine that wrote must not be the one
that confirms.** Every false green in the document happened because success
was reported by the component doing the work.

Live I/O is injected. Without it, a required cell records a known gap with
a pointer rather than being skipped — the kit lands before every contract
passes, and a silent skip is how a gap becomes invisible again.
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

# Why a cell is ❌ today. Contract 4 is the first asserted row: the harness
# exists and is unit-tested; the live backends have not yet recorded a pass.
# The others wait on that same harness with different notebooks.
GAP_REASON = {
    1: "not yet asserted — needs a running notebook session",
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


def record(backend: str, live_write: Callable[[], Result] | None = None) -> list[dict]:
    """Results for one backend. `live_write`, if given, is contract 4 only."""
    if backend not in BACKENDS:
        raise ValueError(f"unknown backend {backend!r}; expected one of {BACKENDS}")
    out: list[dict] = []
    for num, title, pointer in CONTRACTS:
        if (num, backend) in NOT_APPLICABLE:
            out.append(Result(
                id=str(num), contract=title, backend=backend, status="na",
            ).as_dict())
            continue
        if num == 4 and live_write is not None:
            out.append(live_write().as_dict())
            continue
        out.append(Result(
            id=str(num), contract=title, backend=backend, status="gap",
            error=GAP_REASON[num], pointer=pointer,
        ).as_dict())
    return out
