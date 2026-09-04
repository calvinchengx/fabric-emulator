#!/usr/bin/env python3
"""Every parity claim marked supported must name the test that witnesses it.

`docs/24-parity-completion.md` states the rule — *"Every 🟢 needs a real-client
witness in CI"* — but nothing enforced it, and unenforced rules drift. Two
concrete failures this repo already hit:

  * a row claiming external-store support was witnessed for S3 reads only,
    while the same row also covered ADLS Gen2 and Copy *writes*;
  * a Spark row bundled streaming, OPTIMIZE/VACUUM and Java UDFs under one
    verdict, hiding that streaming partly worked.

Both are the same shape: one witness, several claims. This checker makes the
mapping explicit and verifiable.

Witness kinds, deliberately distinguished because they are not equal evidence:

  ci:<job>      a CI job driving a real external client (strongest — this is
                what the rule in doc 24 actually asks for)
  sdk:<Test>    a Go test in which MICROSOFT'S OWN client does the talking —
                go-mssqldb speaking real TDS to the warehouse surface. Third
                party evidence like ci:, but in-process rather than a packaged
                release over a network, so it ranks below it.
  go:<Test>     a Go test: real HTTP, real signed JWTs, real RBAC, but our own
                client rather than a third party's
  boundary:...  the claim is scoped by a documented limitation, with the reason
  TODO          not yet identified — the point of --strict

sdk: is a Go kind, and every rule that says "go" below means the Go FAMILY,
go: and sdk: alike. This matters most at the gate check: gates on Go tests are
DETECTED by scanning the test body, never merely asserted, and moving a test to
sdk: must not quietly buy it the weaker asserted-gate treatment. Five of the
warehouse witnesses are declared gates, so this was not hypothetical.

A witness whose NAME exists is not a witness that RAN. That gap cost real time
twice: `TestWarehouseSQLServerRelayE2E` skips without `WAREHOUSE_MSSQL_DSN`, so
it never ran on a laptop and a security fix broke it undetected until CI; and a
row sat green while the code behind it executed in no test at all. Presence was
all this script ever checked.

So it now also resolves which Go witnesses can SKIP — including transitively,
through a helper like `testsupport.OpenMSSQL` that skips on the caller's behalf,
which is the form no reader spots. A gated witness is legitimate; an UNDECLARED
one is not. Each must be named in the manifest's `_gated` map with its reason,
which makes adding one a deliberate act rather than a silent downgrade of the
evidence behind a green row.

Two rules follow, both enforced under --strict:

  * a gated witness that the manifest does not declare (and, symmetrically, a
    declaration that describes nothing — a stale note is how the map drifts
    back out of step). A declaration goes stale two unrelated ways: the test
    stopped skipping, or it is credited to no SUPPORTED claim. They want
    opposite fixes, so the reason is computed and printed, never assumed;
  * a claim whose witnesses are ALL gated, which is a green row that a default
    `go test ./...` proves nothing about. That count is 0 today; the rule is
    here to keep it there.

What it still does not prove: that a witness ASSERTS the claim, or that the
code behind the claim executes. Coverage answers the second — see AUDIT.md,
where a green row with 0% coverage is recorded — and nothing here answers the
first.

Every file is read as UTF-8 explicitly: the parity map's glyphs are the thing
being matched, and a locale-dependent read turns them into mojibake that matches
nothing while still exiting 0.

Usage:
    check_witnesses.py            report the mapping and exit 0
    check_witnesses.py --strict   also fail on TODO, dangling refs, an
                                  undeclared/stale gate, or a README Real
                                  count that is not the number this prints
"""
import json
import pathlib
import re
import subprocess
import sys

# Windows stdout is cp1252, and this prints text taken from the parity map —
# em dashes, arrows, the glyphs themselves. Reading was fixed to UTF-8 first and
# this was missed, so the same bug simply moved from the input side to the
# output side: `make check` died on UnicodeEncodeError for a single arrow. The
# encoding of what we PRINT is as much a portability question as what we read.
if hasattr(sys.stdout, "reconfigure"):
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")

ROOT = pathlib.Path(__file__).resolve().parent.parent
PARITY = ROOT / "docs" / "parity.md"
MANIFEST = ROOT / "docs" / "witnesses.json"
README = ROOT / "README.md"

# README's glance table types the same figure this script prints. The landing
# page interpolates it; the README was the last typed copy, and it sat at 113
# after the checker had moved to 124. Bound here so a new row cannot leave the
# front page stating a number nothing checks.
#
# The row is MATCHED, not the surrounding sentence: rewriting the Meaning cell
# must not disable the check, and deleting the table must fail rather than
# silently unbind the count.
README_REAL = re.compile(
    r"^\|\s*🟢\s+\*\*Real\*\*\s+\|\s+\*\*(\d+)\*\*\s+\|",
    re.M,
)

# The ecosystem-conformance section is SKIPPED by the claim scanner below, and
# correctly: its rows are clients, not capabilities. But that left its 🟢 marks
# governed by nothing. A client row could say 🟢 while no claim credited the
# suite behind it, and the whole point of a third-party witness is that it
# backs a claim -- delete that CI job and nothing would go red.
#
# Found by review, twice over: `e2e/dbt-fabricspark` runs in five CI jobs,
# is named in that table, and was cited by no claim, while `ci:dbt-fabric`
# was cited twice. The surface it drives -- Livy High-Concurrency -- rested on
# three of our own Go tests plus a Spark Connect suite that speaks a different
# protocol. The strongest available evidence was sitting outside the manifest.
ECOSYSTEM_SECTION = "Ecosystem conformance: real OSS/vendor clients as witnesses"

# Suites named in that table that deliberately credit no claim. Each needs a
# REASON, because "not credited" and "credited to nothing yet" look identical.
# Suites whose CI job is named differently from the directory. Small, explicit
# and self-policing (a mapping to a job that witnesses nothing is reported),
# because deriving it from ci.yml went wrong in a way worth recording: the
# medallion jobs invoke their suites through an `examples/` matrix, not an
# `e2e/` path, so a reachability scan attributed `e2e/medallion` to whichever
# unrelated job happened to mention it.
JOB_FOR = {
    "spark": "spark-a2",
    "livy": "livy-native",
    "eventstream": "eventstream-sail",
}

UNCREDITED = {
    "vscode-extension": (
        "exercises the shared-backend/MWC authoring routes through "
        "api.powerbi.com, which no row in the graded sections covers. Not a "
        "missing citation: a missing claim. See issue #385 (the map states no "
        "denominator) -- the fix is a graded row, and then a citation here."
    ),
}
CI = ROOT / ".github" / "workflows" / "ci.yml"

# Sections that do not make capability claims: the legend, the conformance
# table (itself a list of witnesses), emulator-only helpers, and the explicit
# scope boundary.
SKIP_SECTIONS = {
    "Legend",
    "Ecosystem conformance: real OSS/vendor clients as witnesses",
    "Scope boundary: Fabric, not the predecessor Azure products",
    "Emulator-only (no Fabric equivalent — these exist for testing)",
    "Why the boundary sits where it does",
}


class Unreadable(Exception):
    """The parity map no longer has the shape this checker reads."""


def key_for(feature: str) -> str:
    """A stable-ish key from the row's feature cell: markdown and punctuation
    stripped, lowercased. Rewording a claim changes its key and trips the
    checker — that is intended, since a reworded claim deserves a fresh look
    at whether its witness still covers it."""
    text = re.sub(r"\[([^\]]+)\]\([^)]*\)", r"\1", feature)  # links → text
    text = re.sub(r"[*`_]", "", text)
    text = re.sub(r"[^a-z0-9]+", "-", text.lower())
    return text.strip("-")


def green_claims():
    """Yield (section, feature, key) for every row claiming support.

    The parity map marks support with a glyph, so that is what this matches;
    everything this script PRINTS says "supported" in words.
    """
    section = None
    for line in PARITY.read_text(encoding="utf-8").splitlines():
        if line.startswith("## "):
            section = line[3:].strip()
            continue
        if not line.startswith("| ") or section is None or section in SKIP_SECTIONS:
            continue
        cells = [c.strip() for c in line.strip().strip("|").split("|")]
        if len(cells) < 3:
            continue
        if cells[0] in ("Fabric feature", "Fabric area", "Capability") or set(cells[0]) <= set("-"):
            continue
        if "🟢" in cells[-1]:
            yield section, cells[0], key_for(cells[0])


def ci_job_ids() -> set:
    """Job ids in ci.yml, plus every workflow file's own name.

    Most witnesses name a job in the main pipeline. A few name a whole
    workflow, because that is the unit that exists: `real-fabric` is one
    secret-gated workflow with a single `conformance` job, and crediting
    `ci:conformance` would be ambiguous with any other workflow that names a
    job the same. Both spellings resolve; neither invents one.
    """
    ids = set(re.findall(r"^  ([a-z0-9-]+):$", CI.read_text(encoding="utf-8"), re.M))
    ids.update(p.stem for p in CI.parent.glob("*.yml"))
    return ids


# Function bodies are taken from the `func` header to the next line that starts
# a column-0 `}`. That is not a Go parser, and it does not need to be: it is
# exact for gofmt'd code, which every file here is (CI runs gofmt).
FUNC_RE = re.compile(r"^func (?:\([^)]*\)\s*)?([A-Za-z0-9_]+)\(([^)]*)\)[^{]*\{", re.M)
SKIP_RE = re.compile(r"\.Skipf?\(")


def go_func_bodies() -> dict:
    """name -> list of (signature, body) for every Go func under internal/."""
    out: dict[str, list] = {}
    for path in (ROOT / "internal").rglob("*.go"):
        src = path.read_text(errors="ignore")
        for m in FUNC_RE.finditer(src):
            tail = src[m.end():]
            stop = re.search(r"^\}", tail, re.M)
            body = tail[: stop.start()] if stop else tail
            out.setdefault(m.group(1), []).append((m.group(2), body))
    return out


def gated_go_tests() -> dict:
    """Go test name -> why it can skip.

    Resolved to a fixed point rather than one level deep: `TestReflectDecimalColumn`
    does not skip and contains no gate, it calls `testsupport.OpenMSSQL(t)`, which
    skips for it. A one-level check would miss a helper that fronts another helper,
    and the whole point is that this form is the one a reader does not see.
    """
    bodies = go_func_bodies()
    takes_t = {n for n, vs in bodies.items() if any("testing.T" in sig for sig, _ in vs)}
    gated = {n: "its own t.Skip" for n in takes_t
             if any(SKIP_RE.search(b) for _, b in bodies.get(n, []))}
    changed = True
    while changed:
        changed = False
        for name in takes_t - set(gated):
            for _, body in bodies.get(name, []):
                hit = next((g for g in gated
                            if g != name and re.search(r"\b" + re.escape(g) + r"\(", body)), None)
                if hit:
                    gated[name] = f"{gated[hit].replace('its own t.Skip', 'a t.Skip')} via {hit}()"
                    changed = True
                    break
    return {n: why for n, why in gated.items() if n.startswith("Test")}


def go_test_names() -> set:
    out = subprocess.run(
        ["grep", "-rhoE", r"^func (Test[A-Za-z0-9_]+)", "--include=*_test.go", str(ROOT / "internal")],
        capture_output=True, text=True,
    )
    return {line.split()[1] for line in out.stdout.splitlines() if line.startswith("func ")}


def py_test_names() -> set:
    """Python test modules AND test functions under python/tests.

    `py:` witnesses were counted but never resolved, so one naming a test that
    had been renamed or deleted passed --strict silently — a witness system
    whose whole promise is that a claim names evidence that exists. Both
    spellings are accepted because both are in use: a module (`test_foo`) when
    a whole file is the evidence, a function (`test_foo_does_bar`) when one
    case is.
    """
    tests_dir = ROOT / "python" / "tests"
    if not tests_dir.is_dir():
        return set()
    names = set()
    for path in tests_dir.glob("test_*.py"):
        names.add(path.stem)
        names.update(re.findall(r"^def (test_[A-Za-z0-9_]+)",
                                path.read_text(encoding="utf-8"), re.M))
    return names


def stale_declarations(declared, gated_used, credited, gated_tests, tests,
                       manifest) -> dict:
    """`_gated` entries that describe nothing, each with WHY.

    A declaration is live when the witness is credited to a supported claim AND
    is detected as gated. Failing either makes it stale, but the two failures
    are unrelated and want opposite fixes, and for its first months this check
    reported both as "no longer skips".

    That misdiagnosis cost real time: three T-SQL security tests share one
    `newSecFixture(t)` gate, the parity map got two of the three verdict cells
    left at not-implemented while their prose was rewritten to claim support,
    and the checker answered "these witnesses no longer skip". They skip. They
    were simply credited to claims no longer marked supported. The declarations
    were deleted to clear the gate — exactly backwards, since the map then
    under-declared two genuinely gated witnesses and the red rows stayed red.

    So the reason is computed rather than assumed, and the message names which
    of the two happened.
    """
    keys_for: dict[str, list] = {}
    for key, entry in manifest.items():
        for witness in entry.get("witnesses", []):
            keys_for.setdefault(witness, []).append(key)

    out = {}
    for witness in declared:
        if witness in gated_used:
            continue
        kind, _, name = witness.partition(":")
        keys = keys_for.get(witness, [])
        if witness in credited:
            # Credited to a supported claim, so the gate scan is the authority:
            # go/sdk gates are detected, and not being detected means the
            # t.Skip is gone.
            if kind in ("go", "sdk") and name in tests and name not in gated_tests:
                out[witness] = "no longer skips — the gate is gone from the test. Drop it."
            else:
                out[witness] = "credited but not resolved as gated. Drop it."
        elif not keys:
            out[witness] = ("named by no manifest entry at all — a renamed or deleted "
                            "witness. Drop it.")
        else:
            still = " ".join(sorted(keys))
            gate = ("it still skips" if name in gated_tests else "it no longer skips")
            out[witness] = (
                f"still gated ({gate}) but credited to no SUPPORTED claim: its "
                f"manifest entries ({still}) are not marked supported in the "
                "parity map. Either the row was demoted on purpose — then drop "
                "this declaration too — or a verdict cell was left behind while "
                "the claim moved. Check the map before deleting."
            )
    return out


def ecosystem_suites() -> list[str]:
    """The e2e suite named by each ROW of the ecosystem-conformance table.

    Rows, not the section text: the prose underneath mentions `e2e/type-map`,
    which is a probe another suite invokes rather than a client row of its own.
    Reading the whole section reported it as an uncredited witness, which is
    the sort of false alarm that gets a gate switched off.
    """
    text = PARITY.read_text(encoding="utf-8")
    if "## " + ECOSYSTEM_SECTION not in text:
        # No section, nothing to reconcile. NOT an error here: a synthetic map
        # in a unit test has no ecosystem table and should not be forced to
        # grow one — that is how #367 broke four tests, by adding an invariant
        # its own fixtures could not satisfy. The requirement that THIS repo's
        # map carries the section belongs to the real-repo test, where it can
        # actually be true, and where a rename fails loudly.
        return []
    section = text.split("## " + ECOSYSTEM_SECTION, 1)[1].split("\n## ", 1)[0]
    suites = []
    for line in section.splitlines():
        if not line.startswith("|"):
            continue
        cells = [c.strip() for c in line.strip().strip("|").split("|")]
        if len(cells) < 3 or "🟢" not in cells[-1]:
            continue
        suites += re.findall(r"e2e/([a-z0-9][a-z0-9-]*)", cells[-1])
    if not suites:
        raise Unreadable(
            "the ecosystem-conformance table names no e2e suite in any 🟢 row. The "
            "row format must have changed, and this check is now vacuous.")
    return sorted(set(suites))


def readme_real_mismatch(claimed: int, text: str) -> str | None:
    """None when README's 🟢 Real cell equals `claimed`; otherwise why not.

    Driven from the failing side in the tests: a checker over a README that
    currently matches passes whether or not this function looks at it.
    """
    hits = README_REAL.findall(text)
    if not hits:
        return (
            "README.md has no `| 🟢 **Real** | **N** |` row. The glance table "
            "is a literal bound to the count this checker prints; if the table "
            "is rewritten, this regex has to move with it."
        )
    if len(hits) > 1:
        return (
            f"README.md has {len(hits)} `| 🟢 **Real** | **N** |` rows; "
            "there must be one."
        )
    stated = int(hits[0])
    if stated != claimed:
        return (
            f"README.md says 🟢 Real **{stated}**; this checker counted "
            f"{claimed} supported claims. The landing page interpolates the "
            "same figure; the README was the last typed copy."
        )
    return None


def ecosystem_gaps(manifest: dict) -> list[str]:
    """Every real-client suite in the ecosystem table must back a claim.

    Plus the two declaration maps police themselves: an alias that resolves to
    nothing, or an exemption for a suite that IS credited now, is stale and
    says so. A declaration nothing checks is how the original gap survived.
    """
    cited = {w[3:] for entry in manifest.values()
             if isinstance(entry, dict)
             for w in entry.get("witnesses", []) or [] if w.startswith("ci:")}
    suites = ecosystem_suites()
    if not suites:
        return []  # no table here; see ecosystem_suites for why that is allowed
    problems = []
    for suite in suites:
        job = JOB_FOR.get(suite, suite)
        if suite in UNCREDITED:
            if job in cited:
                problems.append(
                    f"e2e/{suite} is listed in UNCREDITED but ci:{job} now witnesses a "
                    "claim. Drop the exemption; it is describing the past.")
            continue
        if job not in cited:
            problems.append(
                f"e2e/{suite} is a 🟢 client row but no claim cites ci:{job}. Delete "
                "that CI job and nothing would go red — credit it on the rows it "
                "drives, or add it to UNCREDITED with the reason.")
    # Staleness of the two maps, checked only once a table was actually read:
    # against a synthetic map that names none of these suites, every entry
    # would look stale and the checker would fail four unrelated tests.
    for suite, job in sorted(JOB_FOR.items()):
        if job in cited or suite in UNCREDITED or suite not in suites:
            continue
        problems.append(
            f"JOB_FOR maps e2e/{suite} to ci:{job}, which witnesses nothing. Either "
            "the job was renamed again, or the mapping was always wrong.")
    return problems


def main() -> int:
    strict = "--strict" in sys.argv
    manifest = json.loads(MANIFEST.read_text(encoding="utf-8")) if MANIFEST.exists() else {}
    # `_gated` is a declaration, not a claim: witness -> why it can skip.
    declared_gates = manifest.pop("_gated", {})
    jobs, tests, py_tests = ci_job_ids(), go_test_names(), py_test_names()
    gated_tests = gated_go_tests()

    missing, dangling, todo = [], [], []
    # witness -> reason, for the gated witnesses actually credited to a claim.
    gated_used: dict[str, str] = {}
    ungated_by_key: dict[str, int] = {}
    kinds = {"ci": 0, "sdk": 0, "go": 0, "py": 0, "boundary": 0}
    # Which claims lean on each witness — a witness covering many claims is
    # where bundling hides.
    shared: dict[str, list[str]] = {}

    claims = list(green_claims())
    for section, feature, key in claims:
        entry = manifest.get(key)
        if entry is None:
            missing.append((section, feature, key))
            continue
        for witness in entry.get("witnesses", []):
            if witness == "TODO":
                todo.append((section, feature))
                continue
            kind, _, name = witness.partition(":")
            kinds[kind] = kinds.get(kind, 0) + 1
            shared.setdefault(witness, []).append(feature)
            if kind == "ci" and name not in jobs:
                dangling.append(f"{key} → {witness} (no such CI job)")
            elif kind in ("go", "sdk") and name not in tests:
                dangling.append(f"{key} → {witness} (no such Go test)")
            elif kind == "py" and name not in py_tests:
                dangling.append(f"{key} → {witness} (no such Python test)")
            if kind in ("go", "sdk") and name in gated_tests:
                gated_used[witness] = gated_tests[name]
            elif kind not in ("go", "sdk") and witness in declared_gates:
                # A gate this script cannot DETECT, only accept: a CI job that
                # skips on absent secrets is invisible to the Go body scan, and
                # `ci:real-fabric` — the differential leg against a real tenant —
                # is exactly that. Counting it as unconditional evidence would
                # let a claim rest entirely on a job that never runs on a fork.
                # The declaration is the author's assertion; the stale check
                # below cannot police these, which is the price of accepting them.
                #
                # `kind != "go"` IS LOAD-BEARING. Without it a declared Go
                # witness that has STOPPED skipping lands here, enters
                # `gated_used`, and so never appears in `stale` — defeating the
                # stale-declaration rule for precisely the case it was written
                # for. Go gates are detected, never merely asserted.
                gated_used[witness] = "a declared gate this checker cannot detect"
            else:
                # A boundary is not evidence that runs, so it does not count
                # towards a claim having ungated support.
                if kind != "boundary":
                    ungated_by_key[key] = ungated_by_key.get(key, 0) + 1

    # A parse that finds NOTHING is a broken checker, not a clean repo. This
    # ran green on Windows for its whole life while matching zero claims:
    # Path.read_text() uses the LOCALE encoding there, and cp1252 decodes the
    # 🟢 bytes to mojibake without raising, so no row ever matched. Every
    # count below was 0 and every rule vacuously satisfied — the exact shape of
    # failure this script exists to catch, in the script itself.
    if not claims:
        print("FAIL: parsed 0 supported claims from docs/parity.md — the parity")
        print("map is not empty, so this is a parsing failure (encoding? path?),")
        print("and every check below would be vacuously true.")
        return 1

    print(f"supported capability claims: {len(claims)}")
    try:
        readme_text = README.read_text(encoding="utf-8")
    except OSError as exc:
        readme_problem = f"README.md could not be read: {exc}"
    else:
        readme_problem = readme_real_mismatch(len(claims), readme_text)
    if readme_problem:
        print(f"  README Real count                       : {readme_problem}")
    else:
        print(f"  README Real count                       : {len(claims)} (matches)")
    print(f"  witnessed by a real external client (ci:) : {kinds.get('ci', 0)}")
    print(f"  witnessed by Microsoft's own clients (sdk:): {kinds.get('sdk', 0)}")
    print(f"  witnessed by our own Go tests (go:)       : {kinds.get('go', 0)}")
    print(f"  witnessed by our own Python tests (py:)   : {kinds.get('py', 0)}")
    print(f"  scoped by a documented boundary           : {kinds.get('boundary', 0)}")
    print(f"  not yet identified (TODO)                 : {len(todo)}")
    print(f"  absent from the manifest                  : {len(missing)}")
    print(f"  credited witnesses that can SKIP          : {len(gated_used)}")

    undeclared = {w: why for w, why in gated_used.items() if w not in declared_gates}
    stale = stale_declarations(declared_gates, gated_used, set(shared),
                               gated_tests, tests, manifest)
    # A claim with no witness that runs unconditionally is a green row a default
    # `go test ./...` proves nothing about.
    unproven = [(section, feature, key) for section, feature, key in claims
                if key in {k for _, _, k in claims}
                and manifest.get(key) and not ungated_by_key.get(key)
                and any(w in gated_used for w in manifest[key].get("witnesses", []))]

    if gated_used:
        n_undeclared = len(undeclared)
        state = "all declared" if not n_undeclared else f"{n_undeclared} UNDECLARED"
        print(f"\nWitnesses that can skip ({len(gated_used)}, {state}) — a declared")
        print("gate is expected, not an error:")
        # Grouped by reason: the same gate covering nine witnesses is one fact,
        # and printing it nine times buries the one that is undeclared.
        by_reason: dict[str, list] = {}
        for witness, why in sorted(gated_used.items()):
            reason = declared_gates.get(witness, "!! UNDECLARED — add it to `_gated`")
            by_reason.setdefault(reason, []).append((witness, why))
        for reason, entries in by_reason.items():
            for witness, why in entries:
                print(f"  {witness:<46} skips via {why}")
            print(f"      -> {reason}")
    if stale:
        print("\nStale gate declarations:")
        for witness, why in stale.items():
            print(f"  {witness}\n      {why}")
    if unproven:
        print("\nClaims whose every witness can skip — a default test run proves")
        print("nothing about these:")
        for section, feature, key in unproven:
            print(f"  [{section}] {feature[:70]}\n      key: {key}")

    heavy = sorted(((w, c) for w, c in shared.items() if len(c) > 3),
                   key=lambda x: -len(x[1]))
    if heavy:
        print("\nWitnesses carrying many claims (check none is over-credited):")
        for witness, covered in heavy[:5]:
            print(f"  {witness}: {len(covered)} claims")

    try:
        eco = ecosystem_gaps(manifest)
    except Unreadable as exc:
        eco = [str(exc)]
    if eco:
        print("\nEcosystem-conformance suites that back no claim:")
        for problem in eco:
            print(f"  {problem}")

    if missing:
        print("\nClaims with no manifest entry:")
        for section, feature, key in missing[:20]:
            print(f"  [{section}] {feature[:70]}\n      key: {key}")
    if dangling:
        print("\nDangling witness references:")
        for d in dangling:
            print(f"  {d}")

    failed = False
    if strict and (missing or dangling or todo):
        print("\nFAIL: every supported claim needs an identified, existing witness.")
        failed = True
    if strict and undeclared:
        print("\nFAIL: a witness that can skip must be declared in the manifest's")
        print("`_gated` map with the reason. A gated witness is fine; an undeclared")
        print("one silently downgrades the evidence behind a green row.")
        for witness, why in sorted(undeclared.items()):
            print(f'  "{witness}": "<reason>"   (skips via {why})')
        failed = True
    if strict and stale:
        print("\nFAIL: the `_gated` entries above no longer describe a gated witness")
        print("behind a supported claim. Each line says which of the two it is, and")
        print("they want opposite fixes: drop the declaration, or restore the claim.")
        failed = True
    if strict and unproven:
        print("\nFAIL: a claim needs at least one witness that runs unconditionally.")
        failed = True
    if strict and eco:
        print("\nFAIL: a client named in the ecosystem-conformance table must be the")
        print("witness for something. That table is the only place several real-client")
        print("suites are recorded, and nothing reconciled it against the manifest.")
        failed = True
    if strict and readme_problem:
        print("\nFAIL: README.md's 🟢 Real count must match the number this checker")
        print("prints. The glance table is a typed copy of a figure that moves.")
        failed = True
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
