"""Framework-conformance probes: one contract, three backends.

`docs/38-framework-conformance.md` is the contract list. This file is the
assertion shape those contracts take when a backend is asked to prove them.

The first asserted row is contract 4 (write landing), because that is the
failure class that produced silent wrong answers rather than loud failures.
The rule that makes it real: **the engine that wrote must not be the one
that confirms.** Every false green in the document happened because success
was reported by the component doing the work.

A SEAM WORTH KNOWING ABOUT. This module is where sail and jvm assert; the
warehouse column asserts in Go (`internal/server/tds_conformance*_test.go`),
because its contracts are TDS sessions and there is no notebook to carry a
probe. `run.py` records those cells from the Go test verdicts. The rules
here are therefore the NORMATIVE statement of what each contract means —
unit-tested, and what docs/38 describes — but nothing mechanically checks
that the Go legs and these agree. Change one and read the other.

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
    2: "no live session recorded — the offline path cannot read a live signature",
    3: "no live session recorded — a runtime floor is a property of the running image",
    4: "live backends not yet recorded",
    5: "no live fan-out recorded — isolation is a property of concurrent sessions",
    6: "no live statements recorded — fall-through is a property of a running engine",
    7: "no live run recorded — outliving a token needs a session that outlived one",
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


@dataclass(frozen=True)
class SignatureClaim:
    """The signatures one notebook session reported for a module.

    `seen` maps method name to its ordered parameter list. A method the
    session could not find at all is simply absent, which is the case that
    matters most: omission is what a framework reads.
    """

    ok: bool
    seen: dict[str, list[str]] | None = None
    error: str = ""


def signature_shape(
    *,
    session: Callable[[], SignatureClaim],
    reference: dict,
    backend: str,
) -> Result:
    """Contract 2: every parameter real Fabric accepts is present.

    THE ASYMMETRY IS THE CONTRACT, and it is not a style preference. A missing
    parameter fails; an EXTRA one does not. A framework introspects a signature
    and declines to run when a parameter it needs is absent -- without ever
    calling anything -- so omission is a signal it reads. Accepting a parameter
    and ignoring it is correct emulation when there is nothing to switch; the
    emulator has one session and attaches the notebook's own binding.

    ORDER IS PART OF THE SIGNATURE, because Fabric's documented calls are
    positional (`run("Sample1", 90, {"input": 20})`). A runtime with the right
    names in the wrong order accepts that call and does something else with it,
    which is worse than refusing it.
    """
    title = CONTRACTS[1][1]
    pointer = CONTRACTS[1][2]
    claim = session()
    if not claim.ok:
        return Result(id="2", contract=title, backend=backend, status="fail",
                      error=claim.error or "the session reported no signatures",
                      pointer=pointer)

    seen = claim.seen or {}
    missing_methods, wrong = [], []
    for method, spec in sorted(reference.items()):
        if method not in seen:
            missing_methods.append(method)
            continue
        want = spec["params"]
        got = seen[method]
        # Extra trailing parameters are allowed; the documented ones must be
        # present, in order, at the front.
        if got[:len(want)] != want:
            missing = [p for p in want if p not in got]
            if missing:
                wrong.append(f"{method} is missing {', '.join(missing)}")
            else:
                wrong.append(f"{method} has {want} out of order: {got[:len(want)]}")

    problems = []
    if missing_methods:
        problems.append(
            f"{len(missing_methods)} documented method(s) absent: "
            + ", ".join(missing_methods))
    problems.extend(wrong)
    if problems:
        return Result(id="2", contract=title, backend=backend, status="fail",
                      error="; ".join(problems), pointer=pointer)
    return Result(id="2", contract=title, backend=backend, status="pass")


@dataclass(frozen=True)
class RuntimeClaim:
    """What one session reported about the runtime it is running on."""

    ok: bool
    declared: str = ""
    python: str = ""
    spark: str = ""
    error: str = ""


def _at_least(got: str, want: str) -> bool:
    """Version compare on the numeric prefix, without a packaging dependency.

    `3.11.15` meets a `3.11` floor; `3.8.10` does not. Compared as integers so
    3.8 does not sort above 3.11, which is the whole reason this is not a
    string comparison.
    """
    def parts(v):
        out = []
        for chunk in v.split("."):
            digits = ""
            for ch in chunk:
                if not ch.isdigit():
                    break
                digits += ch
            out.append(int(digits) if digits else 0)
        return out

    a, b = parts(got), parts(want)
    a += [0] * (len(b) - len(a))
    b += [0] * (len(a) - len(b))
    return a >= b


def runtime_floor(
    *,
    session: Callable[[], RuntimeClaim],
    runtimes: dict,
    backend: str,
) -> Result:
    """Contract 3: the image declares a Fabric Runtime and MEETS its floor.

    TWO FAILURES, NOT ONE, and they are different problems. An image that
    declares nothing cannot be checked at all -- a framework targeting Runtime
    1.3 has no way to ask whether this is one. An image that declares a runtime
    and ships below its floor is worse: it answers the question wrongly, and
    the first symptom is a missing module long after the agent reported ready,
    which reads as a notebook fault rather than a runtime that was never
    eligible.

    ONLY PYTHON IS ASSERTED. It is the floor that actually broke. Whether the
    engine behaves like Spark 3.5 is the engine matrix's question, which it
    answers row by row rather than by trusting a version string.
    """
    title = CONTRACTS[2][1]
    pointer = CONTRACTS[2][2]
    claim = session()
    if not claim.ok:
        return Result(id="3", contract=title, backend=backend, status="fail",
                      error=claim.error or "the session reported no runtime",
                      pointer=pointer)
    if not claim.declared:
        return Result(
            id="3", contract=title, backend=backend, status="fail",
            error=("the image declares no FABRIC_RUNTIME, so a framework "
                   "targeting a runtime cannot ask whether this is one"),
            pointer=pointer)
    spec = runtimes.get(claim.declared)
    if spec is None:
        return Result(
            id="3", contract=title, backend=backend, status="fail",
            error=(f"the image declares Fabric Runtime {claim.declared!r}, which "
                   f"this kit has no cited floor for: {sorted(runtimes)}"),
            pointer=pointer)
    if not claim.python:
        return Result(id="3", contract=title, backend=backend, status="fail",
                      error="the session reported no Python version",
                      pointer=pointer)
    if not _at_least(claim.python, spec["python"]):
        return Result(
            id="3", contract=title, backend=backend, status="fail",
            error=(f"declares Fabric Runtime {claim.declared} (Python "
                   f"{spec['python']}) and runs Python {claim.python}"),
            pointer=pointer)
    return Result(id="3", contract=title, backend=backend, status="pass")


@dataclass(frozen=True)
class IsolationClaim:
    """What a fan-out of concurrent notebooks reported, one entry each.

    `seen` maps the marker a session was given to what it wrote back:
    `{"marker": ..., "identity": <who it believes it is>}`. A session that
    wrote nothing is absent, which is itself a finding.

    IDENTITY IS PER-SURFACE, and the key is deliberately not called
    "notebook". On a lakehouse it is the notebook id the control plane
    issued the child; on the warehouse there is no notebook — it is the
    warehouse item the session was dialed at, and the database principal the
    relay logged it in as. Both are control-plane-issued and both are what
    leaks if the relay keeps them anywhere process-global.
    """

    ok: bool
    seen: dict[str, dict] | None = None
    error: str = ""


def concurrent_isolation(
    *,
    session: Callable[[], IsolationClaim],
    expected: dict[str, str],
    backend: str,
) -> Result:
    """Contract 5: N children at once, each seeing only its own identity.

    `expected` maps each child's marker to the identity the CONTROL PLANE
    issued it, so the comparison is against what the harness created rather
    than against what another child reported. Two children that leaked into
    each other would agree with each other and disagree with this.

    WHY MARKERS AND IDENTITIES BOTH. A marker alone proves a child ran; the
    identity proves it knew WHICH child it was. The emulator runs one
    long-lived agent with a namespace per session, so everything
    process-global leaks across concurrent
    runs — the prelude's exit-value global did exactly that once, and a
    `runtime.context` built at import would do it again. A child reporting
    another child's identity is that leak, and it is invisible to any
    assertion that only counts successes.
    """
    title = CONTRACTS[4][1]
    pointer = CONTRACTS[4][2]
    claim = session()
    if not claim.ok:
        return Result(id="5", contract=title, backend=backend, status="fail",
                      error=claim.error or "the fan-out reported nothing",
                      pointer=pointer)
    seen = claim.seen or {}
    problems = []
    missing = sorted(set(expected) - set(seen))
    if missing:
        problems.append(
            f"{len(missing)} of {len(expected)} children wrote no findings: "
            + ", ".join(missing))
    for marker, want in sorted(expected.items()):
        got = seen.get(marker)
        if got is None:
            continue
        if got.get("marker") != marker:
            problems.append(
                f"the artifact for {marker} carries marker "
                f"{got.get('marker')!r} — a child wrote another child's file")
        if got.get("identity") != want:
            problems.append(
                f"{marker} believes it is {got.get('identity') or '<empty>'}; "
                f"the control plane issued it {want}")
    # Distinct ids, stated separately: N children all reporting the SAME id
    # would already be caught above, but saying it this way names the leak
    # rather than listing N mismatches.
    ids = [v.get("identity") for v in seen.values() if v.get("identity")]
    if ids and len(set(ids)) != len(ids):
        problems.append(
            f"sessions share an identity: {sorted(ids)}")
    if problems:
        return Result(id="5", contract=title, backend=backend, status="fail",
                      error="; ".join(problems), pointer=pointer)
    return Result(id="5", contract=title, backend=backend, status="pass")


@dataclass(frozen=True)
class FallThroughClaim:
    """Two statements run in one session: one the grammar knows, one it does not."""

    ok: bool
    recognised_ok: bool = False
    recognised_error: str = ""
    unrecognised_ok: bool = False
    unrecognised_error: str = ""
    echo_sent: str = ""
    echo_got: str = ""
    error: str = ""


# Text the agent's own interceptors produce. An unrecognised statement whose
# failure quotes one of these did not fall through — it was rewritten and the
# rewrite failed, which is a different defect wearing the same red.
_INTERCEPTOR_MARKERS = ("delta-rs", "delta_ops", "intercept", "rewrite",
                        "unsupported by the grammar")


def fall_through(
    *,
    session: Callable[[], FallThroughClaim],
    backend: str,
    control: bool,
) -> Result:
    """Contract 6: what the grammar does not know reaches the engine unchanged.

    TWO STATEMENTS, AND BOTH MATTER. The recognised one must SUCCEED, or the
    interception is not installed at all and "it fell through" is vacuous —
    everything falls through when nothing is intercepting. The unrecognised one
    is the contract itself.

    THE CONTROL COLUMN IS WHAT MAKES THIS PROVABLE. On the default engine the
    unrecognised statement must fail, because Sail cannot plan it; on the JVM
    overlay the SAME statement must succeed, because Spark can. A single-engine
    suite cannot tell "the grammar stayed out of the way" from "the grammar
    fired and happened to work" — two engines and one statement can.

    A failure that quotes the agent's own interceptors is refused rather than
    counted: that is a rewrite that failed, not a fall-through.

    THE ECHO IS THE SECOND WITNESS FORM, for a surface that has only one
    engine. The warehouse relays to SQL Server and there is no contrasting
    engine to run the same statement, so the contrast cannot be the witness
    there. Instead the session sends a statement whose payload is a STRING
    LITERAL containing the exact construct the rewriter does recognise, and
    compares what the engine echoed back against what was sent. A rewriter
    that tokenised the literal as SQL changes those bytes; one that stayed out
    of the way cannot. The engine returns the proof, and the emulator cannot
    forge it without reproducing its own input verbatim.

    When `echo_sent` is set it REPLACES the control contrast, because the two
    are alternative answers to the same question and requiring both would make
    the single-engine surface unprovable rather than proven differently.
    """
    title = CONTRACTS[5][1]
    pointer = CONTRACTS[5][2]
    claim = session()
    if not claim.ok:
        return Result(id="6", contract=title, backend=backend, status="fail",
                      error=claim.error or "the session ran neither statement",
                      pointer=pointer)
    problems = []
    if not claim.recognised_ok:
        problems.append(
            "the statement the grammar DOES recognise failed "
            f"({claim.recognised_error[:160]}) — with nothing intercepting, "
            "fall-through proves nothing")
    if claim.echo_sent:
        if claim.echo_got != claim.echo_sent:
            problems.append(
                "the engine echoed back text the session did not send — "
                f"sent {claim.echo_sent!r}, got {claim.echo_got!r}; something "
                "rewrote a statement it does not model")
    elif control:
        if not claim.unrecognised_ok:
            problems.append(
                "the control engine could not run the unrecognised statement "
                f"({claim.unrecognised_error[:160]}); with nothing to contrast, "
                "a pass on the default engine cannot be read as fall-through")
    elif claim.unrecognised_ok:
        problems.append(
            "the unrecognised statement SUCCEEDED on the default engine, which "
            "this engine cannot plan — so something rewrote it")
    else:
        low = claim.unrecognised_error.lower()
        hit = [m for m in _INTERCEPTOR_MARKERS if m in low]
        if hit:
            problems.append(
                f"the failure names the agent's own rewriting ({', '.join(hit)}), "
                "so the statement did not reach the engine unmodified")
        if not claim.unrecognised_error.strip():
            problems.append("the statement failed with no error text to attribute")
    if problems:
        return Result(id="6", contract=title, backend=backend, status="fail",
                      error="; ".join(problems), pointer=pointer)
    return Result(id="6", contract=title, backend=backend, status="pass")


@dataclass(frozen=True)
class CredentialClaim:
    """Two OneLake operations from one session, separated by a token lifetime."""

    ok: bool
    lifetime: int = 0
    slept: float = 0.0
    before_ok: bool = False
    before_error: str = ""
    after_ok: bool = False
    after_error: str = ""
    expiry_checked: bool = False
    expiry_accepted: bool = False
    error: str = ""


def credential_lifetime(
    *,
    session: Callable[[], CredentialClaim],
    backend: str,
) -> Result:
    """Contract 7: a run that outlives the token keeps reading.

    THE FIRST OPERATION IS THE CONTROL. If it fails, nothing about the second
    says anything: a session that could never reach OneLake would "pass" a test
    that only checked the second one failed for the right reason, and fail one
    that only checked it succeeded, for the wrong one.

    THE SLEEP MUST ACTUALLY EXCEED THE LIFETIME. A probe that slept less than the
    token lived would pass on every runtime, including one that never re-mints —
    which is precisely the defect: a token minted at container start, an hour
    later every OneLake read answering 401, and a human restarting by hand
    because it reads as a storage outage. So the session reports both numbers and
    this refuses to grade a run where the gap was never opened.

    AND SURVIVING IS ONLY MEANINGFUL IF EXPIRY IS ENFORCED. "The run kept
    working past the lifetime" and "nothing ever checks a credential" produce
    the identical green, and the second is a security hole wearing the first's
    result. A surface that can present a deliberately expired credential
    reports `expiry_checked`, and this then requires that it was REFUSED. A
    surface that cannot leaves it false and is graded on the wait alone — the
    contract stays what it was, and where the stronger form is available it is
    demanded.
    """
    title = CONTRACTS[6][1]
    pointer = CONTRACTS[6][2]
    claim = session()
    if not claim.ok:
        return Result(id="7", contract=title, backend=backend, status="fail",
                      error=claim.error or "the session ran neither operation",
                      pointer=pointer)
    if not claim.before_ok:
        return Result(
            id="7", contract=title, backend=backend, status="fail",
            error=("the operation BEFORE the wait failed "
                   f"({claim.before_error[:160]}) — with no working baseline the "
                   "second one proves nothing either way"),
            pointer=pointer)
    if claim.lifetime <= 0:
        return Result(
            id="7", contract=title, backend=backend, status="fail",
            error=("the session reported no token lifetime, so it cannot say "
                   "whether the wait outlived one"),
            pointer=pointer)
    if claim.slept <= claim.lifetime:
        return Result(
            id="7", contract=title, backend=backend, status="fail",
            error=(f"slept {claim.slept:.0f}s against a {claim.lifetime}s token "
                   "lifetime — the gap was never opened, so a pass here would "
                   "hold for a runtime that never re-mints"),
            pointer=pointer)
    if not claim.after_ok:
        return Result(
            id="7", contract=title, backend=backend, status="fail",
            error=(f"after {claim.slept:.0f}s, past a {claim.lifetime}s token "
                   f"lifetime, the operation failed: {claim.after_error[:200]}"),
            pointer=pointer)
    if claim.expiry_checked and claim.expiry_accepted:
        return Result(
            id="7", contract=title, backend=backend, status="fail",
            error=("an already-expired credential was ACCEPTED, so outliving a "
                   "lifetime here says nothing about refreshing one — nothing "
                   "is checking expiry at all"),
            pointer=pointer)
    return Result(id="7", contract=title, backend=backend, status="pass")


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
