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
  go:<Test>     a Go test: real HTTP, real signed JWTs, real RBAC, but our own
                client rather than a third party's
  boundary:...  the claim is scoped by a documented limitation, with the reason
  TODO          not yet identified — the point of --strict

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
    declaration for a witness that no longer skips — a stale note is how the
    map drifts back out of step);
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
    check_witnesses.py --strict   also fail on TODO, dangling refs, or an
                                  undeclared/stale gate
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
    return set(re.findall(r"^  ([a-z0-9-]+):$", CI.read_text(encoding="utf-8"), re.M))


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
    kinds = {"ci": 0, "go": 0, "py": 0, "boundary": 0}
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
            elif kind == "go" and name not in tests:
                dangling.append(f"{key} → {witness} (no such Go test)")
            elif kind == "py" and name not in py_tests:
                dangling.append(f"{key} → {witness} (no such Python test)")
            if kind == "go" and name in gated_tests:
                gated_used[witness] = gated_tests[name]
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
    print(f"  witnessed by a real external client (ci:) : {kinds.get('ci', 0)}")
    print(f"  witnessed by our own Go tests (go:)       : {kinds.get('go', 0)}")
    print(f"  witnessed by our own Python tests (py:)   : {kinds.get('py', 0)}")
    print(f"  scoped by a documented boundary           : {kinds.get('boundary', 0)}")
    print(f"  not yet identified (TODO)                 : {len(todo)}")
    print(f"  absent from the manifest                  : {len(missing)}")
    print(f"  credited witnesses that can SKIP          : {len(gated_used)}")

    undeclared = {w: why for w, why in gated_used.items() if w not in declared_gates}
    stale = [w for w in declared_gates if w not in gated_used]
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
        print("\nStale gate declarations (these witnesses no longer skip):")
        for witness in stale:
            print(f"  {witness}")
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
        print("\nFAIL: remove the stale `_gated` entries above — they no longer skip,")
        print("and a stale note is how this map drifts back out of step.")
        failed = True
    if strict and unproven:
        print("\nFAIL: a claim needs at least one witness that runs unconditionally.")
        failed = True
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
