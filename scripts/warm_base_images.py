#!/usr/bin/env python3
"""Pull an e2e suite's base images BEFORE its harness builds them, with a retry
and a second source.

WHY THIS EXISTS. On 2026-10-25 three e2e jobs on `main` and one on a PR failed
inside `docker build`, none of them for a reason in this repository:

    failed to copy: httpReadSeeker: failed open: could not fetch content
    descriptor sha256:7046f4e7... (application/vnd.oci.image.layer.v1.tar+gzip)
    from remote: not found

`mirror.gcr.io` -- Google's pull-through cache of Docker Hub, which every
Dockerfile here has pulled through since #556 -- served `golang:1.27`'s
manifest while answering 404 for layer 3 of 7 of it, for about two hours. The
image had not changed in four days (Docker Hub: last pushed 2026-10-21, same
index digest on both), so it was not a fresh-push race; the cache simply could
not produce a layer it was advertising, and then later it could.

Two properties made that fatal rather than a blip, and this script is shaped by
both:

  * **A cache miss is reported as `not found`, not as a 5xx.** BuildKit treats
    a 404 as final and does not retry it, so a transient upstream failure is
    indistinguishable, to the client, from a layer that does not exist. Retrying
    has to happen out here, around the pull, because nothing inside the build
    ever sees a retryable error.
  * **Retrying alone was already tried, and was not enough.** The governance
    harness has retried `compose up` three times since G44 (e2e/governance/
    stack.py), and it failed all three that morning: 15s and 30s of backoff
    cannot outlast a two-hour window. What outlasts it is a SECOND SOURCE.
    `mirror.gcr.io/<path>` is a content-addressed cache of `docker.io/<path>`,
    so pulling the same reference from Docker Hub and tagging it under the
    mirror's name yields the same bytes under the name the Dockerfile asks for.

WHY PRE-PULLING WORKS AT ALL. With the image in the local store, BuildKit on
the docker driver builds `FROM <ref>` without contacting the registry -- checked
against a registry name that does not resolve (`unreachable.invalid/...`), which
built with `load metadata ... DONE 0.0s`. ci.yml configures no buildx builder,
so every job builds on that driver. A `docker-container` builder would NOT see
the daemon's images, and this would silently stop helping; the test module
pins the absence of `setup-buildx` for that reason.

SERVICE IMAGES TOO. Two of the same morning's failures were not builds at
all: compose PULLING a service image (SeaweedFS for e2e/s3, ttyd for
e2e/portal-terminal) died on `unknown blob`, the same mirror failing the same
way through a different client. Compose's default `pull_policy` is `missing`
and nothing here overrides it, so an image already in the store is not pulled
again -- checked the same way, with a service image named on a registry that
does not resolve: compose created the container without contacting it. So the
same pre-pull covers both, and these are warmed alongside the bases:

  * every `image:` of a service in the suite's OWN compose files, and
  * an `image:` from the ROOT docker-compose.yml only when the suite's Python
    names that service as a string literal -- which is how every harness here
    starts root services (`compose("up", ..., "entra-emulator", "ttyd")`).
    The root file carries OpenMetadata, OpenSearch, Kafka and more behind
    profiles; warming all of it for every suite would pull gigabytes nobody
    runs, so it is the name, not the file, that selects.

Never warmed: a service the suite's compose files give a `build:` (that is how
`fabric-emulator` is built from source rather than pulled from GHCR, and pulling
the published image would be wasted work), and a `${VAR}` reference with no
default, whose value only the harness knows.

WHAT IT WARMS. `--suite NAME` reads e2e/NAME's compose files, takes every
`build:` stanza (`context`, `dockerfile`, defaulting to `Dockerfile`), adds any
Dockerfile physically inside the suite, and collects the literal `FROM`
references of all of them. A suite that builds nothing warms nothing and exits
0 at once, which is what lets ci.yml put this in front of EVERY harness rather
than a hand-kept list of the ones that build -- and a list is exactly the thing
that misses the next suite.

    warm_base_images.py --suite medallion       # what that suite builds
    warm_base_images.py --dockerfile Dockerfile # one Dockerfile's bases
    warm_base_images.py                         # the root Dockerfile's bases

Exit 0 when every base is now local; 1 naming each one that neither source
could supply. Stdlib only, like every guard in scripts/.
"""
from __future__ import annotations

import argparse
import ast
import itertools
import pathlib
import re
import shutil
import subprocess
import sys
import time

ROOT = pathlib.Path(__file__).resolve().parent.parent
E2E = ROOT / "e2e"

MIRROR = "mirror.gcr.io/"
UPSTREAM = "docker.io/"

# Backoff before each attempt. The first is immediate; the rest exist for the
# SHORT failures -- a reset connection, a TLS timeout -- which is all a backoff
# can cover. The long failure is what the second source is for.
BACKOFF = (0, 20, 60)

# `FROM [--platform=...] ref [AS name]`, one instruction per line. Line
# continuations in a FROM are legal and unused in this tree.
FROM = re.compile(
    r"^\s*FROM\s+(?:--platform=\S+\s+)?(?P<ref>\S+)(?:\s+AS\s+(?P<stage>\S+))?\s*$",
    re.IGNORECASE)

# A compose `build:` block: the indented lines under it. Matches the long form
# every compose file here uses; the short form `build: <path>` is read too.
BUILD_BLOCK = re.compile(r"^(?P<indent>[ \t]*)build:[ \t]*(?P<inline>\S.*)?$", re.MULTILINE)


def log(msg: str) -> None:
    print(f"warm_base_images: {msg}", flush=True)


def from_refs(dockerfile: pathlib.Path) -> list[str]:
    """The external images a Dockerfile builds on, in order, without repeats.

    Skipped: a FROM naming an EARLIER STAGE of the same file (that is not an
    image), and a FROM built from an ARG (`FROM ${BASE}`), whose value is only
    known to the build. Neither is pullable from here, and guessing would warm
    the wrong thing.
    """
    stages, refs = set(), []
    for line in dockerfile.read_text(encoding="utf-8").splitlines():
        m = FROM.match(line)
        if not m:
            continue
        ref, stage = m["ref"], m["stage"]
        if ref.lower() in stages or "$" in ref:
            pass
        elif ref not in refs:
            refs.append(ref)
        if stage:
            stages.add(stage.lower())
    return refs


def _context(value: str, compose: pathlib.Path) -> pathlib.Path:
    """A compose build context as a directory.

    A `${VAR}` / `${VAR:-default}` context is the repository root, whatever its
    default says. That form exists in exactly the governance and portal-terminal
    overlays, and both say why in a comment above it: their harness ALWAYS sets
    the variable to the repo root, because "relative contexts resolve against
    the project dir on some compose versions". The `..` default is a fallback
    resolved against the project, not against the overlay file -- read relative
    to the file, it names `e2e/`, which is the mistake the first version of this
    made.
    """
    value = value.strip().strip("'\"")
    if re.fullmatch(r"\$\{[A-Za-z_][A-Za-z0-9_]*(?::?-[^}]*)?\}", value):
        return ROOT
    return (compose.parent / value).resolve()


def _flow_map(inline: str) -> dict:
    """`{ context: ../.., dockerfile: Dockerfile }` as a dict.

    The flow form is how most of the smaller compose files here write a build,
    one line per service. Values in this tree never contain a comma or a brace,
    so splitting is exact for every file it reads; the test module asserts that
    every suite CI runs resolves to Dockerfiles that exist, which is what would
    go red if one ever did.
    """
    body = inline.strip()[1:].rsplit("}", 1)[0]
    out = {}
    for pair in body.split(","):
        key, _, value = pair.partition(":")
        if key.strip():
            out[key.strip()] = value.strip().strip("'\"")
    return out


def compose_dockerfiles(compose: pathlib.Path) -> list[pathlib.Path]:
    """Every Dockerfile a compose file's `build:` stanzas point at."""
    text = compose.read_text(encoding="utf-8")
    lines = text.splitlines()
    out = []
    for m in BUILD_BLOCK.finditer(text):
        inline = (m["inline"] or "").strip()
        if inline.startswith("{"):
            fields = _flow_map(inline)
            if "context" in fields:
                out.append(_context(fields["context"], compose)
                           / fields.get("dockerfile", "Dockerfile"))
            continue
        if inline and not inline.startswith("#"):
            # Short form: `build: ../..` -- a context, Dockerfile by default.
            out.append(_context(inline, compose) / "Dockerfile")
            continue
        start = text.count("\n", 0, m.start()) + 1
        indent = len(m["indent"])
        context, dockerfile = None, "Dockerfile"
        for line in lines[start:]:
            if line.strip() and (len(line) - len(line.lstrip())) <= indent:
                break
            key, _, value = line.strip().partition(":")
            if key == "context":
                context = value
            elif key == "dockerfile":
                dockerfile = value.strip().strip("'\"")
        if context is not None:
            out.append(_context(context, compose) / dockerfile)
    return out


def related_suites(suite: str, _seen=None) -> list[str]:
    """The suite plus every sibling suite whose stack it runs, transitively.

    Three of the medallion harnesses hold no stack of their own: each does
    `sys.path.insert(0, os.path.join(REPO, "e2e", "medallion"))` and runs
    e2e/medallion's. e2e/two-context runs e2e/livy's with
    `os.path.join(HERE, "..", "livy")`. Read from the AST, not by regex over the
    source, because both suites also NAME their siblings in prose, and a
    docstring mentioning a suite is not a stack this one starts. What counts is
    a path join whose string arguments put a suite name straight after `"e2e"`
    or `".."`.
    """
    seen = _seen if _seen is not None else []
    if suite in seen:
        return seen
    seen.append(suite)
    names = {p.name for p in E2E.iterdir() if p.is_dir()}
    for py in sorted((E2E / suite).glob("*.py")):
        try:
            tree = ast.parse(py.read_text(encoding="utf-8"))
        except (SyntaxError, UnicodeDecodeError):
            continue
        for node in ast.walk(tree):
            if not (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
                    and node.func.attr == "join"):
                continue
            lits = [a.value if isinstance(a, ast.Constant) and isinstance(a.value, str) else None
                    for a in node.args]
            for prev, name in itertools.pairwise(lits):
                if prev in ("e2e", "..") and name in names and name != suite:
                    related_suites(name, seen)
    return seen


SERVICE = re.compile(r"^  (?P<name>[A-Za-z0-9][A-Za-z0-9_.-]*):\s*(?:#.*)?$")
IMAGE = re.compile(r"^    image:\s*['\"]?(?P<ref>[^'\"\s#]+)")
BUILD = re.compile(r"^    build:")
VAR = re.compile(r"\$\{(?P<name>[A-Za-z_][A-Za-z0-9_]*)(?::?-(?P<default>[^}]*))?\}")


def _expand(ref: str) -> str | None:
    """`${VAR:-default}` -> default. A variable with no default is unknowable
    here -- only the harness that sets it knows -- so the reference is None."""
    out = ref
    for m in VAR.finditer(ref):
        if m["default"] is None:
            return None
        out = out.replace(m.group(0), m["default"])
    return out


def compose_services(compose: pathlib.Path) -> dict[str, dict]:
    """{service: {"image": ref or None, "build": bool}} for one compose file.

    Reads the two-space service / four-space key layout every compose file in
    this tree uses. The test module asserts the real files parse to services
    with images, so a file that drifts from that layout goes red there rather
    than quietly warming nothing.
    """
    out, svc, in_services = {}, None, False
    for line in compose.read_text(encoding="utf-8").splitlines():
        if re.match(r"^services:\s*$", line):
            in_services, svc = True, None
            continue
        if re.match(r"^\S", line):
            in_services, svc = False, None
            continue
        if not in_services:
            continue
        if m := SERVICE.match(line):
            svc = m["name"]
            out.setdefault(svc, {"image": None, "build": False})
        elif svc and (m := IMAGE.match(line)):
            out[svc]["image"] = m["ref"]
        elif svc and BUILD.match(line):
            out[svc]["build"] = True
    return out


def suite_images(suite: str) -> list[str]:
    """The service images a suite will pull, per the rules in the docstring."""
    if not (E2E / suite).is_dir():
        raise SystemExit(f"warm_base_images: no such suite: e2e/{suite}")
    dirs = [E2E / name for name in related_suites(suite)]
    local: dict[str, dict] = {}
    for compose in sorted(c for d in dirs for c in [*d.glob("*.yml"), *d.glob("*.yaml")]):
        for name, svc in compose_services(compose).items():
            merged = local.setdefault(name, {"image": None, "build": False})
            merged["image"] = svc["image"] or merged["image"]
            merged["build"] = merged["build"] or svc["build"]

    refs = []
    for svc in local.values():
        if svc["image"] and not svc["build"]:
            refs.append(svc["image"])

    py = [p for d in dirs for p in sorted(d.glob("*.py"))]
    source = "\n".join(p.read_text(encoding="utf-8", errors="ignore") for p in py)
    # The ROOT file counts only for a suite that layers it: delta-rs names
    # "fabric-emulator" in log lines while running it as a host binary, and must
    # not be handed the published image for it. Every suite that does layer the
    # root file joins it to the repo root: os.path.join(REPO, "docker-compose.yml").
    layers_root = re.search(r"""\b(?:REPO|ROOT|REPO_ROOT)\b\s*[,/]\s*["']docker-compose\.yml["']""", source)
    root = ROOT / "docker-compose.yml"
    for name, svc in (compose_services(root).items() if root.is_file() else ()):
        named = re.search(rf"""["']{re.escape(name)}["']""", source)
        overridden = name in local and (local[name]["build"] or local[name]["image"])
        if layers_root and named and svc["image"] and not svc["build"] and not overridden:
            refs.append(svc["image"])

    out = []
    for ref in refs:
        ref = _expand(ref)
        if ref and ref not in out:
            out.append(ref)
    return out


def suite_dockerfiles(suite: str) -> list[pathlib.Path]:
    """The Dockerfiles a suite builds: through its compose files, or in it."""
    if not (E2E / suite).is_dir():
        raise SystemExit(f"warm_base_images: no such suite: e2e/{suite}")
    found = []
    for d in (E2E / name for name in related_suites(suite)):
        for compose in sorted([*d.glob("*.yml"), *d.glob("*.yaml")]):
            found.extend(compose_dockerfiles(compose))
        found.extend(sorted(d.glob("Dockerfile*")))
    seen, out = set(), []
    for p in found:
        if p.resolve() not in seen:
            seen.add(p.resolve())
            out.append(p)
    return out


def _docker(*args: str) -> subprocess.CompletedProcess:
    return subprocess.run(["docker", *args], capture_output=True, text=True)


def _pull(ref: str, sleep=time.sleep) -> tuple[bool, str]:
    """`docker pull` with the backoff above. (ok, the last error)."""
    last = ""
    for attempt, delay in enumerate(BACKOFF, 1):
        if delay:
            log(f"{ref}: retrying in {delay}s ({attempt}/{len(BACKOFF)})")
            sleep(delay)
        r = _docker("pull", "--quiet", ref)
        if r.returncode == 0:
            return True, ""
        last = (r.stderr or r.stdout).strip().splitlines()[-1:] or ["(no output)"]
        last = last[0]
        log(f"{ref}: pull failed (attempt {attempt}/{len(BACKOFF)}): {last}")
    return False, last


def warm(ref: str, sleep=time.sleep) -> str | None:
    """Make `ref` present locally. None on success, else why not."""
    ok, err = _pull(ref, sleep)
    if ok:
        log(f"{ref}: ready")
        return None
    if not ref.startswith(MIRROR):
        return f"{ref}: {err}"
    upstream = UPSTREAM + ref[len(MIRROR):]
    log(f"{ref}: the mirror could not supply it; falling back to {upstream}")
    ok, up_err = _pull(upstream, sleep)
    if not ok:
        return f"{ref}: mirror: {err}; Docker Hub: {up_err}"
    tagged = _docker("tag", upstream, ref)
    if tagged.returncode != 0:
        return f"{ref}: pulled {upstream} but could not tag it: {tagged.stderr.strip()}"
    log(f"{ref}: ready (from {upstream}, tagged under the mirror's name)")
    return None


def main(argv=None, sleep=time.sleep) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--suite", action="append", default=[],
                    help="warm what e2e/SUITE builds (repeatable)")
    ap.add_argument("--dockerfile", action="append", default=[],
                    help="warm one Dockerfile's bases (repeatable)")
    args = ap.parse_args(argv)

    dockerfiles = [ROOT / p for p in args.dockerfile]
    for suite in args.suite:
        dockerfiles.extend(suite_dockerfiles(suite))
    if not args.suite and not args.dockerfile:
        dockerfiles.append(ROOT / "Dockerfile")

    refs = []
    for df in dockerfiles:
        if not df.is_file():
            # A build stanza naming a Dockerfile that is not here is the
            # harness's failure to report, at build time, in its own words.
            log(f"skipping {df.relative_to(ROOT) if df.is_relative_to(ROOT) else df}: not a file")
            continue
        for ref in from_refs(df):
            if ref not in refs:
                refs.append(ref)

    for suite in args.suite:
        for ref in suite_images(suite):
            if ref not in refs:
                refs.append(ref)

    if not refs:
        log("nothing to warm — this suite builds and pulls no images")
        return 0
    # Not this script's failure to report. Warming is resilience for a build
    # that is about to happen; where no linux daemon is serving, the build is
    # the step that fails, in its own words. GitHub's Windows runners carry a
    # daemon in WINDOWS-containers mode, where a linux pull would fail and red
    # a job that never builds -- the trap scripts/doctor.sh names for #650.
    if not shutil.which("docker"):
        log(f"docker is not on PATH; leaving {len(refs)} base image(s) to the build")
        return 0
    info = _docker("info", "--format", "{{.OSType}}")
    if info.returncode != 0:
        log(f"no docker daemon reachable; leaving {len(refs)} base image(s) to the build")
        return 0
    if info.stdout.strip() != "linux":
        log(f"the daemon serves {info.stdout.strip() or '?'} containers; every base "
            f"here is linux, so there is nothing it could use")
        return 0

    log(f"{len(refs)} image(s): {', '.join(refs)}")
    problems = [p for p in (warm(ref, sleep) for ref in refs) if p]
    if problems:
        print("warm_base_images: FAIL — neither source could supply:\n  "
              + "\n  ".join(problems), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
