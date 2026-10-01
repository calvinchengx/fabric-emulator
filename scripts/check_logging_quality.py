#!/usr/bin/env python3
"""Every operational log line names its subsystem, and every knob that gates one is FABRIC_*.

WHAT THE LOG IN THIS TREE IS. It is the whole of the account the emulator gives
of its own failure paths. Measured when this landed: 30 `log.*` call sites across
12 non-test Go files, in a tree of 212 non-test Go sources -- and those 30 lines
are where a Livy session reports that its lakehouse did not mount, where a
notebook driver reports that it panicked, where the response recorder reports
that it could not open the file CI is about to upload. There is no metrics
endpoint and no structured sink. When something goes wrong in a compose stack,
this is the evidence.

THE FAILURE SHAPE, in the words this repository has already used twice about it
(python/tests/test_make_check_runs_in_ci.py, and
scripts/check_script_test_coverage.py one directory over): a check that passes is
indistinguishable from a check that is running. A log line is that sentence
turned inside out. Nothing registered which subsystems log at all, so a handler
that stops logging its failure path reads exactly like a handler that has no
failures -- silence where the signal would be, and no diff to notice, because
what is missing is a line that is not there.

THE FOUR INVARIANTS, each one measured against the tree before it was written:

  R1  TAGGED. Every `log.Print*`/`log.Fatal*`/`log.Panic*` whose first argument
      is a string literal must begin `<tag>: ` (or be exactly `<tag>:`, which is
      the `log.Println("tds:", line)` form -- Println supplies the space), where
      <tag> is in the ledger's `subsystems`. A site whose first argument is not a
      literal has no format string to prefix and must be recorded in
      `exemptions` with the reason.

      THE BASELINE: four delimiter conventions coexisted. Bracketed
      (`[onelake-dfs]`, `[onelake-blob]`), colon (`livy:`, `lineage:`, `store:`,
      `warehouse:`, `notebook:`, `eventstream:`, `tds:`), space-delimited
      key=value with no tag at all (`notebook drive job=`, `spark job drive
      job=`), and untagged prose (internal/server/server.go's "closing the
      response recording", internal/server/record.go's
      "FABRIC_RECORD_RESPONSES=%s could not be opened"). So no single grep
      selected one subsystem's lines out of a compose log: `grep onelake` missed
      the colon form, `grep 'onelake-dfs:'` missed the bracketed one, and
      nothing at all selected the two untagged lines.

  R2  NO EXIT FROM A LIBRARY. No `log.Fatal*` or `log.Panic*` outside `cmd/`. A
      library that exits the process cannot be tested, cannot be recovered from,
      and takes its caller's deferred cleanup with it -- in this tree that would
      include the SQLite close in store, and the coverage counters a `go build
      -cover` binary only writes when main RETURNS (see signalStop in
      cmd/fabric-emulator/main.go for what that cost once). Ships at ONE hit, in
      cmd/, recorded: a pure regression guard.

  R3  THE KNOB IS FABRIC_*. Every environment variable that gates log output must
      be named `FABRIC_*`, or be recorded in `legacyEnvNames` with the reason it
      is still read.

      THIS IS THE FINDING THE AUDIT STARTED FROM. `ONELAKE_TRACE` -- read in
      internal/onelake/onelake.go and internal/onelake/blob.go, and the only
      thing that turns on the OneLake request trace -- was the one log-gating
      knob not named FABRIC_*. check_backward_compat.py finds env knobs with
      `_ENV_LITERAL = re.compile(r'"(FABRIC_[A-Z0-9_]+)"')`, a LITERAL scan, so
      it could not see this one: no row in docs/compat-surface.json's `envVars`,
      no `docsUndocumented` reason, no mention in docs/ at all. Its sibling
      FABRIC_TDS_TRACE has both. A knob outside the surface ledger can be
      renamed or deleted with every gate in this repository staying green, which
      is precisely what that ledger exists to prevent. Renaming it to
      FABRIC_ONELAKE_TRACE is what put it inside; this rule is what keeps the
      next one from landing outside.

  R4  --UPDATE TOUCHES ONLY THE DERIVED LIST. `subsystems` is regenerated from
      the tree; `exemptions` and `legacyEnvNames` are hand-written decisions and
      must survive. Otherwise regenerating is how an exemption gets laundered
      into a clean diff -- the refusal check_backward_compat.py's --update makes
      explicit for the same reason.

BOTH DIRECTIONS, like docs/test-flakiness.json and docs/script-test-coverage.json
before it. A tag in the tree and not in the ledger is a stale ledger, fixed with
--update. An entry in the ledger and not in the tree FAILS: a tag nobody emits
goes on blessing a spelling, an exemption whose call site is gone goes on
excusing a file, and a legacy env name nobody reads goes on excusing a knob that
is no longer there. A one-directional ledger only ever grows.

WHAT THIS DELIBERATELY DOES NOT CHECK. Whether a line is TRUE, whether the right
things are logged, and whether a line carries a SEVERITY. The third is a real
finding and it is recorded rather than enforced: every one of the sites is
`log.Printf`/`Println` with no level token, so `eventstream: CreateTopics ...
(topic will auto-create on first produce)` -- benign -- and `livy: lakehouse %s
Files did NOT mount` -- the session is broken -- read identically to a reader and
to a grep. Retrofitting severity across every site, or migrating to `log/slog`,
is a change with its own diff and its own review; docs/64-logging-quality.md
names both as deferred and says why. This gate holds the shape that can be
decided from the source text alone.

Usage:
    check_logging_quality.py            report findings, exit 0
    check_logging_quality.py --strict   exit non-zero on any unrecorded finding
    check_logging_quality.py --update   rewrite the derived `subsystems` list
"""
import argparse
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
LEDGER = ROOT / "docs" / "logging-subsystems.json"

# The three trees that compile into the emulator binary. Deliberately the same
# set check_backward_compat.py scans for env knobs, so the two gates cannot
# disagree about where the process's own source lives.
SOURCES = ("internal", "pkg", "cmd")

# `cmd/` IS the process, so it is the one place exiting is a decision the code
# gets to make. Everything else is a library with a caller.
EXIT_ALLOWED = "cmd"

# The standard logger's whole surface. Fatal* and Panic* are included so R1
# tags them and R2 can locate them; Print* is the ordinary case.
LOG_FUNCS = ("Printf", "Println", "Print", "Fatalf", "Fatalln", "Fatal",
             "Panicf", "Panicln", "Panic")
_LOG_CALL = re.compile(r"\blog\.(" + "|".join(LOG_FUNCS) + r")\s*\(")

# A tag is lower-kebab: one grep, one subsystem, and no shell quoting needed.
_TAG = re.compile(r"^([a-z][a-z0-9]*(?:-[a-z0-9]+)*):(?: |$)")
_TAG_SHAPE = re.compile(r"^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$")

# Every way this tree reads an environment variable. os.* directly, or one of
# internal/config's typed helpers -- the same list check_backward_compat.py's
# _ENV_READ carries, for the same reason: a reader form this parser does not
# know would silently drop a knob out of the scan.
_ENV_READ = re.compile(
    r"\b(?:os\.Getenv|os\.LookupEnv|envOr|envDefault|boolEnv|intEnv|durationEnv"
    r"|maxRequestBytesEnv)\(\s*\"([A-Z][A-Z0-9_]*)\"")

# A knob whose NAME says it gates diagnostic output. Deliberately a name rule
# AND a block rule (see env_log_knobs): the name catches a knob whose guarded
# block is somewhere else, and the block catches one whose name says nothing.
_LOG_KNOB_NAME = re.compile(r"(?:^|_)(?:TRACE|DEBUG|VERBOSE|LOG|LOGGING|SPEW)(?:$|_)")

# What counts as emitting log output inside a guarded block. SetTraceFunc is
# included because internal/server installs the TDS trace hook that way rather
# than calling log itself at that point.
_EMITS_LOG = re.compile(r"\blog\.[A-Z]|SetTraceFunc|SetOutput")


def relkey(path, root=None):
    """A path as the ledger spells it: repo-relative, forward slashes.

    The separator is not cosmetic and the reasoning is inherited rather than
    invented -- check_test_flakiness.py shipped `str(path.relative_to(ROOT))`
    and produced `cmd\\fabric-emulator\\main.go` against the forward-slash
    spelling in the checked-in JSON. A both-directions checker does not MISS
    things when that happens; it reports everything twice, every site as
    untagged and every entry as stale, on one platform only.

    FALLS BACK TO THE ABSOLUTE PATH rather than raising, which is the same
    guard check_backward_compat._rel carries and for the same measured reason:
    `relative_to` RAISES on a path outside the root, and the root is exactly
    what a test monkeypatches to a tmp_path. Without the fallback the error
    REPORTER crashes on the malformed-ledger case it exists to report -- found
    by three tests in python/tests/test_check_logging_quality.py, which is the
    argument for having written them.
    """
    p = path if isinstance(path, pathlib.PurePath) else pathlib.PurePath(path)
    base = ROOT if root is None else root
    b = base if isinstance(base, pathlib.PurePath) else pathlib.PurePath(base)
    if not p.is_absolute():
        return p.as_posix()
    try:
        return p.relative_to(b).as_posix()
    except ValueError:
        return p.as_posix()


def go_sources(root=None):
    """Non-test Go sources under internal/, pkg/ and cmd/, sorted.

    Tests are excluded because a test's log line is scaffolding, not the
    emulator's account of itself: internal/api/livy_catalog_test.go redirects
    the logger precisely so it can read a production line back, and holding its
    own helpers to the production convention would be holding the wrong file to
    it.
    """
    base = ROOT if root is None else root
    out = []
    for name in SOURCES:
        d = base / name
        if not d.is_dir():
            continue
        out += [p for p in d.rglob("*.go") if not p.name.endswith("_test.go")]
    return sorted(out)


def strip_and_find(text):
    """Scan Go source, yielding (line, func, first_literal, call_text) per call.

    A GO-AWARE SCANNER RATHER THAN A REGEX OVER THE RAW TEXT, and the first
    thing it bought was a corrected baseline. The audit's own opening grep --
    `grep -rnE 'log\\.(Printf|Println|Print|Fatal|Panic)'` -- reported 31 call
    sites, and one of them was this comment in cmd/fabric-emulator/main.go:

        // clean shutdown -- and, because main would then log.Fatal, os.Exit would

    A checker counting that would have reported an untagged log.Fatal inside a
    sentence explaining log.Fatal, with no call there to fix. So comments and
    string bodies are skipped as text rather than matched around: the number is
    30, and the guard's findings name code.

    `first_literal` is the decoded value of the leading string argument, with
    adjacent `+`-joined literals folded together (several sites in this tree
    wrap a long format string that way), or None when the first argument is not
    a literal -- `log.Fatal(err)` passes a value, not a format. `call_text` is
    the call expression with whitespace collapsed; the ledger keys exemptions on
    it rather than on a line number, because a line number goes stale the moment
    anything above it moves and that would train a reader to re-run --update
    without reading the diff.
    """
    i, n, line = 0, len(text), 1
    while i < n:
        c = text[i]
        if c == "\n":
            line += 1
            i += 1
            continue
        if text.startswith("//", i):
            while i < n and text[i] != "\n":
                i += 1
            continue
        if text.startswith("/*", i):
            end = text.find("*/", i + 2)
            end = n if end < 0 else end + 2
            line += text.count("\n", i, end)
            i = end
            continue
        if c == "`":
            end = text.find("`", i + 1)
            end = n if end < 0 else end + 1
            line += text.count("\n", i, end)
            i = end
            continue
        if c == '"':
            i = _skip_interpreted(text, i)
            continue
        if c == "'":
            j = i + 1
            while j < n and text[j] != "'":
                j += 2 if text[j] == "\\" else 1
            i = min(j + 1, n)
            continue
        m = _LOG_CALL.match(text, i)
        if m:
            lit, _ = _leading_literal(text, m.end())
            yield line, m.group(1), lit, _call_text(text, i, m.end())
            i = m.end()
            continue
        i += 1


def _skip_interpreted(text, i):
    """Index just past the interpreted string literal starting at `i`."""
    j = i + 1
    while j < len(text):
        if text[j] == "\\":
            j += 2
            continue
        if text[j] in ('"', "\n"):
            return j + 1
        j += 1
    return len(text)


_ESCAPES = {"n": "\n", "t": "\t", "r": "\r", '"': '"', "\\": "\\", "'": "'",
            "a": "\a", "b": "\b", "f": "\f", "v": "\v", "0": "\0"}


def _unescape(raw):
    """Decode the escapes a Go interpreted literal can carry.

    Only the ones a log format string plausibly holds. A `\\u` or `\\x` escape
    is left as written, which is safe in the only direction that matters here:
    an undecoded escape cannot accidentally LOOK like a valid `tag: ` prefix,
    so the worst case is a finding a human reads rather than a pass nobody does.
    """
    out, i = [], 0
    while i < len(raw):
        if raw[i] == "\\" and i + 1 < len(raw):
            out.append(_ESCAPES.get(raw[i + 1], raw[i:i + 2]))
            i += 2
            continue
        out.append(raw[i])
        i += 1
    return "".join(out)


def _leading_literal(text, start):
    """The decoded leading string argument at `start`, and where it ends.

    Adjacent literals joined by `+` fold into one value: several sites here wrap
    a long format string across lines that way (record.go's
    "FABRIC_RECORD_RESPONSES=%s could not be opened, so nothing " + "will be
    recorded: %v"), and reading only the first fragment would be reading half
    the message. Returns (None, start) when the first argument is not a literal.
    """
    i, n, parts = start, len(text), []
    while i < n and text[i] in " \t\r\n":
        i += 1
    if i >= n or text[i] != '"':
        return None, start
    while True:
        end = _skip_interpreted(text, i)
        parts.append(_unescape(text[i + 1:end - 1]))
        j = end
        while j < n and text[j] in " \t\r\n":
            j += 1
        if j < n and text[j] == "+":
            j += 1
            while j < n and text[j] in " \t\r\n":
                j += 1
            if j < n and text[j] == '"':
                i = j
                continue
        return "".join(parts), end


def _call_text(text, start, after_open):
    """The call expression, parens matched, whitespace collapsed.

    Nesting-aware and literal-aware, so `log.Printf("a(b", x)` and
    `log.Printf("%v", f(g()))` both end where they really end rather than at the
    first `)`.
    """
    i, depth, n = after_open, 1, len(text)
    while i < n and depth:
        c = text[i]
        if c == '"':
            i = _skip_interpreted(text, i)
            continue
        if c == "`":
            end = text.find("`", i + 1)
            i = n if end < 0 else end + 1
            continue
        if c == "(":
            depth += 1
        elif c == ")":
            depth -= 1
        i += 1
    return re.sub(r"\s+", " ", text[start:i]).strip()


def log_sites(root=None):
    """Every log call in the scanned trees, as dicts the rules read."""
    sites = []
    for path in go_sources(root):
        text = path.read_text(encoding="utf-8")
        rel = relkey(path, root)
        for line, func, lit, call in strip_and_find(text):
            sites.append({"file": rel, "line": line, "func": func,
                          "literal": lit, "call": call,
                          "tag": tag_of(lit)})
    return sites


def tag_of(literal):
    """The subsystem tag a format string declares, or None.

    `<tag>: ` is the form, and bare `<tag>:` is accepted as well because
    `log.Println("tds:", line)` supplies the separating space itself -- the
    reader and the grep see the identical `tds: ...` either way, so refusing it
    would be enforcing the implementation rather than the output.
    """
    if not literal:
        return None
    m = _TAG.match(literal)
    return m.group(1) if m else None


def env_log_knobs(root=None):
    """Env variables that gate log output, as (file, line, name) triples.

    TWO RULES, because each catches what the other cannot and a knob needs only
    one of them to be a diagnostic lever:

      BY NAME   the identifier says what it is -- TRACE, DEBUG, VERBOSE, LOG.
                This is what catches a knob read in one place and consumed in
                another, where no block analysis could see the connection.

      BY BLOCK  the `if` it guards emits log output. Narrow on purpose: only an
                `if` whose condition contains the read and whose `{` is on that
                same line, brace-matched from there. That is the exact shape
                both OneLake trace sites use. Broadening it means guessing at
                statement extent with a brace counter, which is what gives
                check_test_flakiness.py's Go pass its false positives -- and a
                false positive here would be an argument about the checker
                rather than a fix.

    Neither rule can see a knob with an innocent name whose log output is
    emitted elsewhere. That is a real limit and it is written down rather than
    papered over: R3 is a gate on the naming convention, and what makes it worth
    having is that the convention is now checkable at all.
    """
    found = {}
    for path in go_sources(root):
        text = path.read_text(encoding="utf-8")
        rel = relkey(path, root)
        for m in _ENV_READ.finditer(text):
            name = m.group(1)
            line = text.count("\n", 0, m.start()) + 1
            if not (_LOG_KNOB_NAME.search(name) or _guards_log(text, m.start())):
                continue
            found.setdefault((rel, line, name), None)
    return sorted(found)


def _guards_log(text, at):
    """Does the env read at `at` sit in an `if` whose body emits log output?"""
    bol = text.rfind("\n", 0, at) + 1
    eol = text.find("\n", at)
    eol = len(text) if eol < 0 else eol
    stmt = text[bol:eol]
    if not re.match(r"\s*if\b", stmt) or not stmt.rstrip().endswith("{"):
        return False
    i, depth, n = text.index("{", at), 0, len(text)
    while i < n:
        if text[i] == "{":
            depth += 1
        elif text[i] == "}":
            depth -= 1
            if not depth:
                break
        i += 1
    return bool(_EMITS_LOG.search(text[text.index("{", at):i]))


def load_ledger(path=None):
    """The ledger, validated.

    A MALFORMED LEDGER FAILS LOUDLY rather than reading as empty, which is the
    whole reason this is a function and not three `data.get` calls at the call
    site. `json.loads` raises on a truncated file by itself; what it will not
    catch is an exemption with no `reason`, which parses fine and suppresses a
    finding while explaining nothing -- an omission wearing a decision's
    clothes, in test_make_check_runs_in_ci.py's words about its own LOCAL_ONLY.
    """
    p = LEDGER if path is None else path
    if not p.is_file():
        raise FileNotFoundError(
            f"{relkey(p)} does not exist. Create it with --update, then write "
            "the `exemptions` and `legacyEnvNames` entries by hand.")
    data = json.loads(p.read_text(encoding="utf-8"))

    subsystems = data.get("subsystems", [])
    if not isinstance(subsystems, list):
        raise ValueError(f"{relkey(p)}: `subsystems` must be a list")
    for tag in subsystems:
        if not isinstance(tag, str) or not _TAG_SHAPE.match(tag):
            raise ValueError(
                f"{relkey(p)}: {tag!r} is not a subsystem tag. A tag is "
                "lower-kebab -- one grep, one subsystem, and nothing a shell "
                "needs quoting for.")

    exemptions = {}
    for entry in data.get("exemptions", []):
        if not isinstance(entry, dict):
            raise ValueError(f"{relkey(p)}: every exemption must be an object, "
                             f"got {entry!r}")
        missing = [k for k in ("file", "call", "reason") if not entry.get(k)]
        if missing:
            raise KeyError(
                f"a logging exemption is missing {', '.join(missing)}: "
                f"{entry!r}. Every entry needs the file, the call it exempts "
                "and the reason -- an exemption with no reason suppresses a "
                "finding while explaining nothing, which is an omission "
                "wearing a decision's clothes.")
        key = (entry["file"], re.sub(r"\s+", " ", entry["call"]).strip())
        if key in exemptions:
            raise ValueError(
                f"{relkey(p)}: {key[0]} {key[1]} is exempt twice; two reasons "
                "for one call means one of them is not being read")
        exemptions[key] = entry

    legacy = {}
    for entry in data.get("legacyEnvNames", []):
        if not isinstance(entry, dict):
            raise ValueError(f"{relkey(p)}: every legacyEnvNames entry must be "
                             f"an object, got {entry!r}")
        missing = [k for k in ("name", "reason") if not entry.get(k)]
        if missing:
            raise KeyError(
                f"a legacyEnvNames entry is missing {', '.join(missing)}: "
                f"{entry!r}. A knob excused with no reason is how a name nobody "
                "decided to keep goes on being kept.")
        if entry["name"] in legacy:
            raise ValueError(f"{relkey(p)}: {entry['name']} is recorded twice")
        legacy[entry["name"]] = entry
    return {"subsystems": sorted(subsystems), "exemptions": exemptions,
            "legacy": legacy, "raw": data}


def scan(root=None, ledger=None):
    """Every finding, by kind. An empty value means the rule is satisfied.

    untagged        R1: a literal-first call with no tag, or a tag the ledger
                    does not know.
    exits           R2: log.Fatal*/Panic* outside cmd/.
    unprefixed_env  R3: a log knob not named FABRIC_* and not recorded.
    stale_tags      a ledgered tag nothing emits.
    stale_exempt    an exemption whose call site is gone.
    stale_legacy    a recorded legacy env name nothing reads.
    """
    led = load_ledger() if ledger is None else ledger
    known, exempt = set(led["subsystems"]), led["exemptions"]
    sites = log_sites(root)

    untagged, emitted, matched = [], set(), set()
    for s in sites:
        key = (s["file"], s["call"])
        if key in exempt:
            matched.add(key)
            continue
        if s["tag"] is None:
            untagged.append(dict(s, why=(
                "no subsystem tag" if s["literal"] is not None
                else "the first argument is not a string literal, so there is "
                     "no format string to tag")))
        elif s["tag"] not in known:
            untagged.append(dict(s, why=f"tag {s['tag']!r} is not in the ledger"))
        else:
            emitted.add(s["tag"])

    exits = [s for s in sites
             if s["func"].startswith(("Fatal", "Panic"))
             and s["file"].split("/")[0] != EXIT_ALLOWED]

    unprefixed_env = [
        {"file": f, "line": ln, "name": name}
        for f, ln, name in env_log_knobs(root)
        if not name.startswith("FABRIC_") and name not in led["legacy"]]

    read_names = {name for _, _, name in env_log_knobs(root)}
    return {
        "untagged": untagged,
        "exits": exits,
        "unprefixed_env": unprefixed_env,
        "stale_tags": sorted(known - emitted),
        "stale_exempt": sorted(set(exempt) - matched),
        "stale_legacy": sorted(n for n in led["legacy"] if n not in read_names),
        "sites": sites,
        "emitted": sorted(emitted),
    }


def write_ledger(emitted, previous, path=None):
    """Rewrite the ledger, regenerating ONLY the derived `subsystems` list.

    `exemptions` and `legacyEnvNames` are decisions someone wrote down, and
    --update must not be able to erase them: otherwise regenerating is how an
    exemption gets laundered into a clean diff, which is the refusal
    check_backward_compat.py's --update makes explicit for the same reason. The
    `_comment` block is preserved too -- it carries the measured baseline, and a
    regenerated file that dropped the reasoning would leave the next reader with
    a list and no account of why it exists.
    """
    p = LEDGER if path is None else path
    out = dict(previous.get("raw", {})) if previous else {}
    out["subsystems"] = sorted(emitted)
    out["counts"] = {
        "subsystems": len(out["subsystems"]),
        "exemptions": len(out.get("exemptions", [])),
        "legacyEnvNames": len(out.get("legacyEnvNames", [])),
    }
    # Key order is fixed so --update produces a reviewable diff rather than a
    # reshuffled file: the derived list sits between the prose and the two
    # hand-written maps, where the diff reads as what it is.
    order = ["_comment", "counts", "subsystems", "exemptions", "legacyEnvNames"]
    ordered = {k: out[k] for k in order if k in out}
    ordered.update({k: v for k, v in out.items() if k not in ordered})
    p.write_text(json.dumps(ordered, indent=2) + "\n", encoding="utf-8")


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--strict", action="store_true",
                    help="exit non-zero on any unrecorded finding")
    ap.add_argument("--update", action="store_true",
                    help="rewrite the derived `subsystems` list from the tree")
    args = ap.parse_args(argv)

    # A SWEEP THAT WALKED NOTHING REPORTS SUCCESS, which is the shape every
    # checker in this directory was written about. Both flakiness passes and
    # check_script_test_coverage.py carry the same two guards: no sources, or no
    # sites, means the scan stopped matching rather than that the tree is clean.
    sources = go_sources()
    if not sources:
        print("check_logging_quality: walked zero Go sources under "
              f"{', '.join(SOURCES)} -- a check that inspects nothing passes "
              "vacuously. Either the packages moved, or the glob stopped "
              "matching.", file=sys.stderr)
        return 1

    try:
        previous = load_ledger() if LEDGER.is_file() else None
    except (ValueError, KeyError) as err:
        print(f"check_logging_quality: {err}", file=sys.stderr)
        return 1

    if args.update:
        if previous is None:
            previous = {"raw": {}, "subsystems": [], "exemptions": {}, "legacy": {}}
        # Derived from every tag the tree ACTUALLY emits, including tags the
        # ledger does not yet know -- that is what makes --update the fix for a
        # stale ledger. Exempt sites contribute nothing, by construction: they
        # have no tag to contribute.
        emitted = sorted({s["tag"] for s in log_sites()
                          if s["tag"] and (s["file"], s["call"])
                          not in previous["exemptions"]})
        if not emitted:
            print("check_logging_quality: --update resolved NO subsystem tags, "
                  "so it would write an empty list and the gate would go on "
                  "passing having stopped measuring. Teach strip_and_find the "
                  "form the tree now uses.", file=sys.stderr)
            return 1
        write_ledger(emitted, previous)
        print(f"check_logging_quality: ledger rewritten -- {len(emitted)} "
              f"subsystem(s): {', '.join(emitted)}")
        return 0

    if previous is None:
        print(f"check_logging_quality: no ledger at {relkey(LEDGER)}. Create "
              "one with --update.", file=sys.stderr)
        return 1

    found = scan(ledger=previous)
    sites = found["sites"]
    if not sites:
        print(f"check_logging_quality: found zero log call sites across "
              f"{len(sources)} Go source(s). The tree had 30 when this gate "
              "landed, so the scanner has stopped matching rather than the "
              "logging having gone away.", file=sys.stderr)
        return 1

    if not args.strict:
        for s in found["untagged"]:
            print(f"UNTAGGED  {s['file']}:{s['line']} log.{s['func']} -- {s['why']}")
        for s in found["exits"]:
            print(f"EXIT      {s['file']}:{s['line']} log.{s['func']} outside "
                  f"{EXIT_ALLOWED}/")
        for e in found["unprefixed_env"]:
            print(f"ENVNAME   {e['file']}:{e['line']} {e['name']} is not FABRIC_*")
        for tag in found["stale_tags"]:
            print(f"STALE     subsystem {tag!r} is in the ledger and nothing emits it")
        for f, call in found["stale_exempt"]:
            print(f"STALE     exemption {f} {call} -- that call site is gone")
        for name in found["stale_legacy"]:
            print(f"STALE     legacy env name {name} -- nothing reads it")
        print(f"\ncheck_logging_quality: {len(sites)} log call site(s) across "
              f"{len({s['file'] for s in sites})} file(s); "
              f"{len(found['emitted'])} subsystem(s) emitted; "
              f"{len(previous['exemptions'])} exemption(s), "
              f"{len(previous['legacy'])} legacy env name(s)")
        return 0

    problems = []
    if found["untagged"]:
        problems.append(
            f"{len(found['untagged'])} log call site(s) do not name a subsystem "
            f"from {relkey(LEDGER)}:\n    " + "\n    ".join(
                f"{s['file']}:{s['line']} log.{s['func']} -- {s['why']}"
                for s in found["untagged"]) + "\n"
            "  A log line is the only account this emulator gives of its own "
            "failure paths, and an untagged one cannot be selected out of a "
            "compose log by any grep. Prefix the format string with "
            "`<subsystem>: `, add the tag to the ledger with --update if it is "
            "new, or record the site in `exemptions` with the reason.")
    if found["exits"]:
        problems.append(
            f"{len(found['exits'])} log.Fatal/Panic call(s) outside "
            f"{EXIT_ALLOWED}/:\n    " + "\n    ".join(
                f"{s['file']}:{s['line']} log.{s['func']}"
                for s in found["exits"]) + "\n"
            "  A library that exits the process cannot be tested, cannot be "
            "recovered from, and takes its caller's deferred cleanup with it -- "
            "here that is the SQLite close in store, and the coverage counters a "
            "`go build -cover` binary writes only when main RETURNS. Return an "
            f"error and let {EXIT_ALLOWED}/ decide.")
    if found["unprefixed_env"]:
        problems.append(
            f"{len(found['unprefixed_env'])} environment knob(s) gate log output "
            "and are not named FABRIC_*:\n    " + "\n    ".join(
                f"{e['file']}:{e['line']} {e['name']}"
                for e in found["unprefixed_env"]) + "\n"
            "  check_backward_compat.py finds env knobs by scanning for the "
            "FABRIC_* literal, so a knob named anything else is invisible to the "
            "surface ledger: no envVars row, no docsUndocumented reason, and a "
            "rename or deletion that every gate here reports as green. That is "
            "exactly how ONELAKE_TRACE went unledgered. Rename it FABRIC_*, or "
            "record it in `legacyEnvNames` with the reason it is still read.")
    stale = ([f"subsystem {t!r} is in the ledger and nothing emits it"
              for t in found["stale_tags"]]
             + [f"exemption {f} {c} -- that call site is gone"
                for f, c in found["stale_exempt"]]
             + [f"legacy env name {n} -- nothing reads it"
                for n in found["stale_legacy"]])
    if stale:
        problems.append(
            f"{len(stale)} ledger entr(y/ies) record something the tree no "
            "longer has; a stale entry goes on blessing a spelling or excusing "
            "a file, and would silently re-cover it if the code came back:\n    "
            + "\n    ".join(stale) + "\n"
            "  Delete the entries (or run --update for the derived list).")

    if problems:
        print("check_logging_quality: " + "\n\n".join(problems), file=sys.stderr)
        return 1

    print(f"check_logging_quality: {len(sites)} log call site(s) across "
          f"{len({s['file'] for s in sites})} file(s), every one naming one of "
          f"{len(found['emitted'])} subsystem(s); no log.Fatal/Panic outside "
          f"{EXIT_ALLOWED}/; every log knob FABRIC_*-prefixed "
          f"({len(previous['legacy'])} legacy name(s) recorded)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
