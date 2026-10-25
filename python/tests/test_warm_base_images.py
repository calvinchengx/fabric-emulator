"""scripts/warm_base_images.py: what it reads, what it pulls, and where CI runs it.

Three layers, because each can rot on its own. The PARSING is driven against
fixtures and then against the real tree, since a regex that quietly stops
matching a compose form warms nothing and still exits 0. The PULLING is driven
at its one subprocess boundary, with no docker and no network, the way
test_cron_workflow_freshness.py drives its own. The WIRING is read out of
ci.yml, because a step that exists in a script and in no job is a step nobody
runs.
"""
from __future__ import annotations

import pathlib
import re
import subprocess
import sys
import types

import pytest
import yaml

REPO = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO / "scripts"))
import warm_base_images as w  # noqa: E402

CI = REPO / ".github" / "workflows" / "ci.yml"
HARNESS = re.compile(r"python\s+e2e/([a-z0-9_-]+)/\S+\.py")
STEP = "Warm the images this job builds and pulls"


# --- FROM -----------------------------------------------------------------------


def test_from_refs_strips_platform_and_skips_stage_and_arg_references(tmp_path):
    df = tmp_path / "Dockerfile"
    df.write_text(
        "# FROM commented/out:1\n"
        "ARG BASE=x\n"
        "FROM --platform=$BUILDPLATFORM mirror.gcr.io/library/golang:1.27 AS build\n"
        "RUN true\n"
        "FROM ${BASE}\n"
        "FROM build AS again\n"
        "FROM gcr.io/distroless/static-debian12:nonroot\n"
        "FROM mirror.gcr.io/library/golang:1.27\n",
        encoding="utf-8")
    assert w.from_refs(df) == [
        "mirror.gcr.io/library/golang:1.27",
        "gcr.io/distroless/static-debian12:nonroot",
    ]


def test_the_root_dockerfile_builds_on_the_two_images_the_incident_was_about():
    """Pins the subject. golang:1.27 is the image whose layer the mirror could
    not serve; if this stops reading it, the warm step no longer covers the one
    failure it was written for."""
    assert w.from_refs(REPO / "Dockerfile") == [
        "mirror.gcr.io/library/golang:1.27",
        "gcr.io/distroless/static-debian12:nonroot",
    ]


# --- compose build stanzas ------------------------------------------------------


def _compose(tmp_path, text):
    d = tmp_path / "e2e" / "suite"
    d.mkdir(parents=True)
    f = d / "docker-compose.yml"
    f.write_text(text, encoding="utf-8")
    return f


def test_every_build_form_the_tree_uses_is_read(tmp_path, monkeypatch):
    monkeypatch.setattr(w, "ROOT", tmp_path)
    f = _compose(tmp_path, (
        "services:\n"
        "  long:\n"
        "    build:\n"
        "      # a comment inside the block\n"
        "      context: ../..\n"
        "      dockerfile: docker/sail/Dockerfile\n"
        "      args: { UV_GROUP: runtime }\n"
        "    image: x\n"
        "  default:\n"
        "    build:\n"
        "      context: .\n"
        "  flow:\n"
        "    build: { context: ../.., dockerfile: Dockerfile }\n"
        "  short:\n"
        "    build: ../..\n"
        "  env:\n"
        "    build:\n"
        "      context: ${GOV_BUILD_CONTEXT:-..}\n"))
    got = [p.relative_to(tmp_path).as_posix() for p in w.compose_dockerfiles(f)]
    assert got == [
        "docker/sail/Dockerfile",          # long form, explicit dockerfile
        "e2e/suite/Dockerfile",            # long form, default dockerfile
        "Dockerfile",                      # flow form
        "Dockerfile",                      # short form
        "Dockerfile",                      # ${VAR:-..} is the ROOT, not e2e/
    ]


def test_an_env_var_context_is_the_root_even_when_its_default_says_dot_dot(tmp_path, monkeypatch):
    """The first version read `${GOV_BUILD_CONTEXT:-..}` relative to the overlay
    file and resolved `e2e/Dockerfile`, which does not exist -- so governance,
    one of the four jobs this was written for, would have warmed nothing."""
    monkeypatch.setattr(w, "ROOT", tmp_path)
    f = _compose(tmp_path, "s:\n  build:\n    context: ${TERM_BUILD_CONTEXT:-..}\n")
    assert w.compose_dockerfiles(f) == [tmp_path / "Dockerfile"]


def _ci_suites():
    text = "\n".join(line for line in CI.read_text(encoding="utf-8").splitlines()
                     if not line.strip().startswith("#"))
    return sorted(set(HARNESS.findall(text)))


def test_every_suite_ci_runs_resolves_to_dockerfiles_that_exist():
    """The parser against the real tree. A build stanza resolving to a path
    that is not there is the parser being wrong, not the suite."""
    missing = [(s, p.relative_to(REPO).as_posix())
               for s in _ci_suites() for p in w.suite_dockerfiles(s) if not p.is_file()]
    assert missing == []


def test_the_real_tree_has_something_to_warm():
    """Non-vacuity for the test above: if every suite resolved to NOTHING it
    would pass too. 35 of the 52 suites CI runs built images when this landed;
    the floor sits well under that so adding or retiring a suite is not a red."""
    building = [s for s in _ci_suites()
                if any(p.is_file() and w.from_refs(p) for p in w.suite_dockerfiles(s))]
    assert len(building) >= 25, building
    assert "governance" in building and "vscode-extension" in building


def test_an_unknown_suite_is_refused_by_name():
    with pytest.raises(SystemExit, match="no such suite: e2e/not-a-suite"):
        w.suite_dockerfiles("not-a-suite")


# --- service images ------------------------------------------------------------


def test_compose_services_reads_image_and_build_per_service(tmp_path):
    f = tmp_path / "docker-compose.yml"
    f.write_text(
        "x-anchor: &a\n"
        "  image: not/a-service:1\n"
        "services:\n"
        "  pulled:\n"
        "    image: mirror.gcr.io/chrislusf/seaweedfs:latest  # trailing comment\n"
        "  built:\n"
        "    image: ghcr.io/x/built:latest\n"
        "    build:\n"
        "      context: .\n"
        "  bare:\n"
        "    command: [\"true\"]\n"
        "volumes:\n"
        "  data:\n", encoding="utf-8")
    assert w.compose_services(f) == {
        "pulled": {"image": "mirror.gcr.io/chrislusf/seaweedfs:latest", "build": False},
        "built": {"image": "ghcr.io/x/built:latest", "build": True},
        "bare": {"image": None, "build": False},
    }


def test_a_variable_resolves_to_its_default_and_none_without_one():
    assert w._expand("ghcr.io/x/e:${V:-0.9.0}") == "ghcr.io/x/e:0.9.0"
    assert w._expand("ghcr.io/x/e:${V-0.9.0}") == "ghcr.io/x/e:0.9.0"
    assert w._expand("ghcr.io/x/e:${V}") is None


@pytest.fixture
def tree(tmp_path, monkeypatch):
    """A fake repository: a root compose file and suites beside it."""
    monkeypatch.setattr(w, "ROOT", tmp_path)
    monkeypatch.setattr(w, "E2E", tmp_path / "e2e")
    (tmp_path / "docker-compose.yml").write_text(
        "services:\n"
        "  entra-emulator:\n    image: ghcr.io/x/entra:${ENTRA:-0.9.0}\n"
        "  fabric-emulator:\n    image: ghcr.io/x/fabric:latest\n"
        "  ttyd:\n    image: mirror.gcr.io/tsl0922/ttyd:1.7.7\n"
        "  openmetadata:\n    image: ghcr.io/x/om:1\n", encoding="utf-8")

    def suite(name, files):
        d = tmp_path / "e2e" / name
        d.mkdir(parents=True)
        for fname, text in files.items():
            (d / fname).write_text(text, encoding="utf-8")
    return suite


LAYERS_ROOT = 'COMPOSE = ["-f", os.path.join(REPO, "docker-compose.yml")]\n'


def test_a_root_service_is_warmed_only_when_named_by_a_suite_that_layers_the_root_file(tree):
    tree("terminal", {
        "run.py": LAYERS_ROOT + 'compose("up", "entra-emulator", "fabric-emulator", "ttyd")\n',
        "build-override.yml": "services:\n  fabric-emulator:\n    build:\n      context: ${TERM_BUILD_CONTEXT:-..}\n",
    })
    # ttyd and entra: named, not built. fabric-emulator: the overlay BUILDS it,
    # so its published image would be wasted work. openmetadata: not named.
    assert w.suite_images("terminal") == ["ghcr.io/x/entra:0.9.0", "mirror.gcr.io/tsl0922/ttyd:1.7.7"]


def test_a_name_in_a_suite_that_does_not_layer_the_root_file_warms_nothing(tree):
    """e2e/delta-rs names "fabric-emulator" in log lines and runs it as a host
    binary. Handing it the published image would be a pull of nothing it uses."""
    # delta-rs's own line: the name as a STANDALONE literal, which is what the
    # name rule matches. A fixture that buried it inside a longer string would
    # pass with the layering check deleted -- this one's first version did.
    tree("host-binary", {"run.py": 'BIN = os.path.join(WORK, "fabric-emulator" + EXE)\n'})
    assert w.suite_images("host-binary") == []


def test_a_suites_own_definition_of_a_service_wins_over_the_root_one(tree):
    tree("own", {
        "run.py": LAYERS_ROOT + 'compose("up", "entra-emulator")\n',
        "docker-compose.yml": "services:\n  entra-emulator:\n    image: ghcr.io/x/entra:own\n"
                              "  seaweedfs:\n    image: mirror.gcr.io/chrislusf/seaweedfs:latest\n",
    })
    assert w.suite_images("own") == ["ghcr.io/x/entra:own", "mirror.gcr.io/chrislusf/seaweedfs:latest"]


def test_a_sibling_whose_stack_a_suite_runs_is_followed_and_prose_is_not(tree):
    """medallion-advanced holds no stack: it imports e2e/medallion's run.py
    through a path join. Its docstring ALSO names e2e/other, which it does not
    run -- a regex would follow both; the AST follows the join only."""
    tree("medallion", {"docker-compose.yml": "services:\n  sql:\n    image: mcr.microsoft.com/mssql/server:2022-latest\n"})
    tree("other", {"docker-compose.yml": "services:\n  nope:\n    image: x/never:1\n"})
    tree("medallion-advanced", {"run.py": (
        '"""Runs the e2e/medallion stack; see also e2e/other and "other"."""\n'
        'sys.path.insert(0, os.path.join(REPO, "e2e", "medallion"))\n')})
    tree("two-context", {"run.py": 'BASE = os.path.join(HERE, "..", "medallion")\n'})
    assert w.related_suites("medallion-advanced") == ["medallion-advanced", "medallion"]
    assert w.related_suites("two-context") == ["two-context", "medallion"]
    assert w.suite_images("medallion-advanced") == ["mcr.microsoft.com/mssql/server:2022-latest"]


def test_the_two_service_images_that_failed_are_warmed_by_their_suites():
    """The incident, against the real tree: e2e/s3's SeaweedFS and
    e2e/portal-terminal's ttyd died on `unknown blob` while compose pulled them."""
    # Exact lists rather than memberships: stronger, and no registry host in an
    # `in` test for CodeQL to read as URL sanitisation (it flagged that shape
    # twice today, #85 here and #58/#82 in check_entra_install.py).
    assert w.suite_images("s3") == [
        "ghcr.io/calvinchengx/entra-emulator:0.9.0",
        "mirror.gcr.io/chrislusf/seaweedfs:latest",
    ]
    assert w.suite_images("portal-terminal") == [
        "ghcr.io/calvinchengx/entra-emulator:0.9.0",
        "mirror.gcr.io/tsl0922/ttyd:1.7.7",
    ]
    assert w.suite_images("delta-rs") == []


def test_the_stackless_medallion_harnesses_warm_the_medallion_stack():
    """Before sibling-following these three warmed NOTHING: no compose file of
    their own, so the old reader found no builds and no services."""
    stack = set(w.suite_images("medallion"))
    bases = {r for p in w.suite_dockerfiles("medallion") if p.is_file() for r in w.from_refs(p)}
    assert stack and bases
    for s in ("medallion-advanced", "medallion-dbt-fabricspark", "medallion-advanced-dbt-fabricspark"):
        assert stack <= set(w.suite_images(s)), s
        assert bases <= {r for p in w.suite_dockerfiles(s) if p.is_file() for r in w.from_refs(p)}, s


# --- pulling --------------------------------------------------------------------


def _done(rc=0, out="", err=""):
    return types.SimpleNamespace(returncode=rc, stdout=out, stderr=err)


@pytest.fixture
def docker(monkeypatch):
    """Script `docker` by command. `fail[ref]` is how many pulls of ref fail."""
    calls = []

    def fake(argv, **kw):
        assert argv[0] == "docker"
        calls.append(argv[1:])
        verb = argv[1]
        if verb == "pull":
            ref = argv[-1]
            left = fake.fail.get(ref, 0)
            if left:
                fake.fail[ref] = left - 1
                return _done(1, err=f"Error response from daemon: {ref}: not found")
            return _done()
        if verb == "tag":
            return _done(fake.tag_rc, err="tag refused" if fake.tag_rc else "")
        if verb == "info":
            return _done(fake.info_rc, out=fake.ostype)
        return _done()

    fake.fail, fake.tag_rc, fake.info_rc, fake.ostype = {}, 0, 0, "linux\n"
    fake.calls = calls
    monkeypatch.setattr(subprocess, "run", fake)
    monkeypatch.setattr(w.subprocess, "run", fake)
    monkeypatch.setattr(w.shutil, "which", lambda _n: "/usr/bin/docker")
    return fake


MIRRORED = "mirror.gcr.io/library/golang:1.27"
UPSTREAM = "docker.io/library/golang:1.27"


def test_a_healthy_pull_is_one_call_and_no_wait(docker):
    slept = []
    assert w.warm(MIRRORED, sleep=slept.append) is None
    assert docker.calls == [["pull", "--quiet", MIRRORED]]
    assert slept == []


def test_a_short_failure_is_absorbed_by_the_backoff(docker):
    docker.fail[MIRRORED] = 1
    slept = []
    assert w.warm(MIRRORED, sleep=slept.append) is None
    assert slept == [w.BACKOFF[1]]
    assert all(c[0] == "pull" for c in docker.calls), "fell back when a retry sufficed"


def test_a_mirror_that_cannot_serve_falls_back_to_docker_hub_under_the_same_name(docker):
    """The incident: the mirror 404s on every attempt. Docker Hub supplies the
    same content-addressed image, and the tag is what lets the unchanged
    Dockerfile's `FROM mirror.gcr.io/...` find it locally."""
    docker.fail[MIRRORED] = len(w.BACKOFF)
    assert w.warm(MIRRORED, sleep=lambda _s: None) is None
    assert ["pull", "--quiet", UPSTREAM] in docker.calls
    assert docker.calls[-1] == ["tag", UPSTREAM, MIRRORED]


def test_a_non_mirror_reference_has_no_second_source(docker):
    ref = "mcr.microsoft.com/azure-cli:2.89.0-azurelinux3.0"
    docker.fail[ref] = len(w.BACKOFF)
    err = w.warm(ref, sleep=lambda _s: None)
    assert err and err.startswith(ref)
    # EXACTLY the primary attempts and nothing after them -- no second source,
    # no tag. Stronger than asserting "docker.io" appears in no call, and not
    # that shape: CodeQL reads a bare host in a substring test as URL
    # sanitisation (py/incomplete-url-substring-sanitization), the finding
    # #611 retired from check_entra_install.py the same way.
    assert docker.calls == [["pull", "--quiet", ref]] * len(w.BACKOFF)


def test_both_sources_failing_names_both(docker):
    docker.fail[MIRRORED] = docker.fail[UPSTREAM] = len(w.BACKOFF)
    err = w.warm(MIRRORED, sleep=lambda _s: None)
    assert err is not None
    assert "mirror:" in err and "Docker Hub:" in err
    assert not any(c[0] == "tag" for c in docker.calls)


def test_a_tag_that_fails_is_a_failure_not_a_success(docker):
    """Without the tag, the image is local under a name no Dockerfile asks for,
    and the build goes back to the mirror -- so 'pulled' is not 'ready'."""
    docker.fail[MIRRORED] = len(w.BACKOFF)
    docker.tag_rc = 1
    err = w.warm(MIRRORED, sleep=lambda _s: None)
    assert err and "could not tag it" in err


# --- main -----------------------------------------------------------------------


def test_a_suite_that_builds_nothing_touches_no_docker(docker, capsys):
    """Why ci.yml can put the step in front of every harness: for the 17
    suites that build nothing, it costs a parse."""
    assert w.main(["--suite", "delta-rs"], sleep=lambda _s: None) == 0
    assert docker.calls == []
    assert "nothing to warm" in capsys.readouterr().out


def test_main_warms_and_succeeds(docker, capsys):
    assert w.main(["--suite", "vscode-extension"], sleep=lambda _s: None) == 0
    pulled = [c[-1] for c in docker.calls if c[0] == "pull"]
    # The exact set the suite's three Dockerfiles build on, rather than two
    # memberships: stronger, and no host string in an `in` test for CodeQL to
    # read as URL sanitisation.
    assert set(pulled) == {
        MIRRORED,
        "gcr.io/distroless/static-debian12:nonroot",
        "mirror.gcr.io/library/python:3.14-slim",
        # The suite's one pulled service, warmed since service images are.
        "ghcr.io/calvinchengx/entra-emulator:0.9.0",
    }


def test_main_fails_naming_what_neither_source_could_supply(docker, capsys):
    docker.fail[MIRRORED] = docker.fail[UPSTREAM] = len(w.BACKOFF)
    assert w.main([], sleep=lambda _s: None) == 1
    err = capsys.readouterr().err
    assert "neither source could supply" in err and MIRRORED in err


@pytest.mark.parametrize("which, info_rc, ostype, said", [
    (None, 0, "linux\n", "not on PATH"),
    ("/usr/bin/docker", 1, "", "no docker daemon reachable"),
    ("/usr/bin/docker", 0, "windows\n", "serves windows containers"),
])
def test_no_linux_daemon_is_left_to_the_build(docker, monkeypatch, capsys, which, info_rc, ostype, said):
    """Not this script's failure to report: where nothing can run a linux
    image, the build is the step that fails, in its own words. A Windows
    runner's daemon is in windows-containers mode, and a linux pull there
    would red a job that never builds."""
    monkeypatch.setattr(w.shutil, "which", lambda _n: which)
    docker.info_rc, docker.ostype = info_rc, ostype
    assert w.main([], sleep=lambda _s: None) == 0
    assert said in capsys.readouterr().out
    assert not any(c[0] == "pull" for c in docker.calls)


# --- the wiring -----------------------------------------------------------------


def _jobs():
    return yaml.safe_load(CI.read_text(encoding="utf-8"))["jobs"]


MATRIX_HARNESS = re.compile(r"python\s+e2e/(\$\{\{\s*matrix\.\w+\s*\}\})/\S+\.py")
LOOP = re.compile(r"for\s+(\w+)\s+in\s+([a-z0-9 _-]+?)\s*;\s*do")


def _job_suites(job):
    """The `--suite` arguments a job's warm step must carry.

    Three forms, because #666 shipped recognising only the first and missed a
    job of each of the other two -- including the medallion matrix, one of the
    four jobs whose failure that change existed for:

      * a literal harness, `python e2e/sail/run.py`
      * a matrix-templated one, `python e2e/${{ matrix.harness }}/run.py`,
        whose warm step must carry the SAME template so each leg resolves its
        own suite
      * a shell loop, `for suite in a b c; do ... e2e/$suite/run.py`
    """
    runs = "\n".join(s.get("run", "") for s in job.get("steps", []))
    runs = "\n".join(line for line in runs.splitlines() if not line.strip().startswith("#"))
    found = set(HARNESS.findall(runs))
    found |= {re.sub(r"\s+", " ", m) for m in MATRIX_HARNESS.findall(runs)}
    for var, names in LOOP.findall(runs):
        if re.search(rf"e2e/\$\{{?{re.escape(var)}\}}?/", runs):
            found |= set(names.split())
    return sorted(found)


def test_every_job_that_runs_a_harness_warms_first_and_names_exactly_its_suites():
    """Asserting the list, not the instances (test_make_check_runs_in_ci.py's
    argument): the 58th harness job lands with no warm step and nothing says so
    otherwise. Position matters as much as presence -- second step, straight
    after checkout, so it precedes every build in the job, including the ones
    some jobs run before their harness."""
    problems = []
    for jid, job in _jobs().items():
        suites = _job_suites(job)
        if not suites:
            continue
        steps = job["steps"]
        warm = [i for i, s in enumerate(steps) if s.get("name") == STEP]
        if warm != [1] or "actions/checkout" not in steps[0].get("uses", ""):
            problems.append(f"{jid}: warm step at {warm}, want exactly [1] after checkout")
            continue
        step = steps[1]
        got = sorted(re.findall(r"--suite (\$\{\{[^}]*\}\}|\S+)", step["run"]))
        if got != suites:
            problems.append(f"{jid}: warms {got}, runs {suites}")
        if step.get("if") != "runner.os == 'Linux'":
            problems.append(f"{jid}: warm step not Linux-guarded")
    assert problems == []


def test_the_harness_forms_the_wiring_reads_are_all_present():
    """Non-vacuity for the three forms: if ci.yml stopped using one, the
    recogniser for it would be untested dead weight; if the recogniser broke,
    the job it covers would silently drop out of the invariant above -- which
    is how the medallion matrix went unwarmed."""
    forms = {"literal": 0, "matrix": 0, "loop": 0}
    for job in _jobs().values():
        runs = "\n".join(s.get("run", "") for s in job.get("steps", []))
        forms["literal"] += bool(HARNESS.search(runs))
        forms["matrix"] += bool(MATRIX_HARNESS.search(runs))
        forms["loop"] += any(re.search(rf"e2e/\$\{{?{re.escape(v)}\}}?/", runs) for v, _ in LOOP.findall(runs))
    assert all(forms.values()), forms


def test_every_matrix_templated_harness_resolves_to_real_suites():
    """`--suite ${{ matrix.harness }}` is checked by the runner, not here, so
    the matrix values themselves must be suites that exist -- a typo would
    otherwise surface as `no such suite` on one leg only."""
    for jid, job in _jobs().items():
        runs = "\n".join(s.get("run", "") for s in job.get("steps", []))
        for key in re.findall(r"e2e/\$\{\{\s*matrix\.(\w+)\s*\}\}/", runs):
            matrix = job["strategy"]["matrix"]
            values = matrix.get(key) or [leg[key] for leg in matrix.get("include", [])]
            assert values, f"{jid}: matrix.{key} has no values"
            for v in values:
                assert (REPO / "e2e" / v / "run.py").is_file(), f"{jid}: matrix.{key}={v} is not a suite"


def test_ci_builds_on_the_docker_driver():
    """Pre-pulling helps ONLY because the default docker driver builds from the
    daemon's image store. A `docker-container` builder (setup-buildx) keeps its
    own store, would not see a warmed image, and this whole step would become a
    no-op that still reports success."""
    text = CI.read_text(encoding="utf-8")
    assert "setup-buildx-action" not in text
    assert "buildx create" not in text


def test_the_go_module_download_is_retried_and_still_fails_closed():
    """The other half of the incident: proxy.golang.org resetting HTTP/2 streams
    inside `go mod download`. The retry must be bounded and must still end the
    build with a non-zero exit when every attempt fails."""
    df = (REPO / "Dockerfile").read_text(encoding="utf-8")
    assert not re.search(r"^RUN go mod download\s*$", df, re.M), "bare, unretried download"
    assert "for attempt in 1 2 3 4" in df and "go mod download && exit 0" in df
    assert "giving up\" >&2; exit 1" in df
