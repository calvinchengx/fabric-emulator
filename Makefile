# Thin wrappers over the docker compose + scripts workflow. The compose files
# remain the source of truth; this exists so the everyday cycle is one word
# each. Nothing here is required — every target shows the command it runs.
#
#   make up      # start the stack (governance profile: OpenMetadata + ingest)
#   make status  # is the stack actually usable? (exit non-zero if not)
#   make clean   # stop everything AND delete the data volumes
#
# Linux, macOS and Windows. On Windows the recipes still run under a POSIX
# shell — `sh.exe` from Git for Windows, which also supplies the grep/awk/curl
# the scripts use. Install once and everything below works from PowerShell or
# cmd:
#
#   winget install Git.Git         # provides sh.exe + grep/awk/cut/curl
#   winget install ezwinports.make # GNU Make itself (no admin needed)
#
# `make doctor` checks the whole toolchain and prints what is missing.
#
# The governance profile is on by default so `make up` matches what the
# quickstart advertises; override with PROFILE= to run the lean stack.
# Both real runtimes are on by default, because both back a first-class Fabric
# item type: the catalog (OpenMetadata) and Data workflows (Apache Airflow).
# `--profile` is REPEATABLE — a comma-joined value is read as one profile name
# that matches nothing, and COMPOSE_PROFILES (the env var) is the comma one.
PROFILE ?= --profile governance --profile airflow
# The overlay travels WITH the profile, and this conditional is what makes that
# true rather than merely intended. Both halves are needed and neither works
# alone:
#   --profile airflow              starts the scheduler
#   -f docker-compose.airflow.yml  hands the emulator its URL and DAG folder
#
# Listing the overlay UNCONDITIONALLY breaks `make up PROFILE=`: the overlay
# makes fabric-emulator depend on `airflow`, and with no profile that service is
# not in the project at all — "depends on undefined service", before a single
# container starts. Wiring the URL in the base file instead is the dual bug this
# repo already shipped once (the medallion compose set FABRIC_SPARK_AGENT_URL
# while spark-agent sat behind a profile, so the emulator drove notebooks at a
# container nobody started). Coupling them is the only arrangement where the
# lean stack answers an honest `AirflowNotConfigured`.
#
# Every target uses this one variable so the -f list never varies FOR A GIVEN
# PROFILE: compose hashes the configuration it is HANDED, and a shorter list
# recreates running containers.
AIRFLOW_OVERLAY = $(if $(findstring --profile airflow,$(PROFILE)),-f docker-compose.airflow.yml,)
COMPOSE  = docker compose $(PROFILE) \
             -f docker-compose.yml \
             -f docker-compose.override.yml \
             $(AIRFLOW_OVERLAY)

# Windows: force the recipes onto sh.exe. GNU Make on Windows falls back to
# cmd.exe when it cannot find a shell, and cmd cannot run a single line of what
# is below. Make searches PATH for this itself, so the spaces in
# "C:\Program Files\Git\bin" are its problem, not ours.
ifeq ($(OS),Windows_NT)
  SHELL := sh.exe
  .SHELLFLAGS := -c
endif

# Same interpreter resolution as scripts/status.sh, deliberately — otherwise
# `make spark` and `make status-spark` run the SAME spark_check.py under two
# different Pythons.
#
# uv first: every Python entry point in this repo runs through it, so the
# project environment is the only interpreter the code is tested against.
#
# The bare fallback still matters on a machine without uv, and there locating an
# interpreter is not enough to know one exists: on Windows `python3` is normally
# the Microsoft Store *alias stub*, which sits on PATH (so `command -v python3`
# succeeds) and then exits 49 telling you to install from the Store. Run each
# candidate and take the first that executes. Override with PY= for anything
# else. Unquoted on use below, because the uv form is several words.
PY ?= $(shell if command -v uv >/dev/null 2>&1; then echo "uv run --frozen --no-sync python"; \
	else for c in python3 python py; do if "$$c" -c '' >/dev/null 2>&1; then echo "$$c"; break; fi; done; fi)

.PHONY: help doctor up up-lite up-jupyter up-jvm up-eventstream dax-linux down restart clean status status-spark spark logs ps seed test test-race check lint docs-build docs-serve

help: ## Show the available targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  %-14s %s\n", $$1, $$2}'

doctor: ## Check the toolchain and the docker context this Makefile needs
	@sh scripts/doctor.sh

up: ## Start the whole stack in the background
	@$(COMPOSE) up -d || { sh scripts/port_conflict.sh; exit 1; }

up-lite: ## Contract-only pair — no compute sidecars, honest 501s on Spark/SQL
	docker compose -f docker-compose.yml up -d

up-jupyter: ## Start the stack plus JupyterLab on :8888 (a real notebook editor)
	docker compose --profile jupyter -f docker-compose.yml -f docker-compose.override.yml up -d
	@echo "JupyterLab: http://localhost:8888"

up-jvm: ## Swap the default Sail engine for JVM Spark (RDD, streaming sinks, JVM UDFs)
	docker compose -f docker-compose.yml -f docker-compose.override.yml -f docker-compose.spark-jvm.yml up -d

up-eventstream: ## Sail (default) + Kafka broker for Eventstream notebook API
	docker compose --profile eventstream -f docker-compose.yml -f docker-compose.override.yml -f docker-compose.eventstream.yml up -d

dax-linux: ## Linux+KVM only: dockur/windows for an msmdsrv guest (docs/52)
	@test "$$(uname -s)" = Linux || { echo "dax-linux is Linux/KVM only. On macOS use UTM; on Windows run Desktop on the host. See docs/52-msmdsrv-hosts.md" >&2; exit 1; }
	@test -e /dev/kvm || { echo "dax-linux needs /dev/kvm. Use Docker Engine on the metal, not Docker Desktop / OrbStack / Rancher Desktop on a Mac. See docs/52-msmdsrv-hosts.md" >&2; exit 1; }
	docker compose -f e2e/msmdsrv/docker-compose.yml up -d

down: ## Stop and remove containers (volumes SURVIVE)
	$(COMPOSE) down

clean: ## Stop and remove containers AND delete the data volumes (full reset)
	$(COMPOSE) down -v

restart: clean up ## Full reset: clean, then start again

status: ## Report whether the stack is usable (non-zero exit if not)
	@sh scripts/status.sh

status-spark: ## status, plus a real Livy session executing Spark statements
	@sh scripts/status.sh --spark

spark: ## Deep Spark check only (Livy -> spark-agent -> sail)
	@test -n "$(PY)" || { echo "no uv and no working python (tried python3, python, py); set PY=" >&2; exit 1; }
	$(PY) scripts/spark_check.py

seed: ## Catalog the emulator into OpenMetadata (seeds a demo if empty)
	$(COMPOSE) run --rm govern-ingest

ps: ## Container states for this project
	$(COMPOSE) ps

logs: ## Tail logs (SVC=<service> to narrow)
	$(COMPOSE) logs -f --tail 100 $(SVC)

# The same two commands CI runs as the `ruff + ty` job, in the same order, both
# configured in pyproject.toml. Local-only because they were CI-only: `make
# check` covered the invariant scripts and nothing looked at the ~1800
# statements of agent, shim and script Python until a push.
#
# ruff lints .ipynb sources too, which is how a notebook with its imports in the
# wrong order reached CI as a red X on a green branch.
#
# `check` must stay runnable with nothing but Python (see PY above), and ruff
# arrives through a uv dependency group. So a machine without uv gets a LOUD
# skip: a quiet one would make "check passed" and "lint never ran" look alike.
lint: ## ruff + ty over the Python sources — the CI lint job, locally
	@if command -v uv >/dev/null 2>&1; then \
	  uv run --frozen --group lint ruff check . && \
	  uv run --frozen --group lint --group test --group spark-client ty check; \
	else \
	  echo "lint SKIPPED: no uv on PATH — CI still runs ruff + ty" >&2; \
	fi

check: lint ## Repo invariants — the checks that used to exist only in CI
	@$(PY) scripts/check_witnesses.py --strict
	@$(PY) scripts/casefiles.py --check
	@$(PY) scripts/check_notebookutils_surface.py --strict
	@$(PY) scripts/check_runtime_wiring.py --strict
	@$(PY) scripts/check_e2e_matrix.py --strict
	@# Also run by docs-build, which is where it gates the site build. Here
	@# too because a broken docs link is a repo invariant, and `make check`
	@# is what someone runs before pushing: without it the only signal is
	@# the docs job in CI, a full cycle later. Cost is a few hundred ms.
	@$(PY) scripts/check_docs_links.py --strict
	@$(PY) scripts/check_govern_types.py
	@$(PY) scripts/check_example_parity.py
	@$(PY) scripts/check_example_portability.py
	@$(PY) scripts/check_conformance.py --strict
	@# THE ONE CONTRACT GATE THAT NEEDS NO RECORDING, which is why it is
	@# here and its three siblings are not. Conformance, route coverage
	@# and the surface ledger all read a recording, so they live in the
	@# aggregate CI job behind eleven e2e suites and cannot answer before
	@# a push. This one reads the Go source and the vendored swagger:
	@# offline, deterministic, and it is the direction that bites hardest
	@# -- a route the emulator answers that no spec documents is a URL a
	@# script binds to here and 404s on in production.
	@$(PY) scripts/check_undocumented_routes.py --strict
	@# THE FIFTH READING OF THE SAME REGISTRATIONS, and the only one
	@# that asks whether a surface a RELEASED BINARY ACCEPTED still
	@# exists. The four above all point from a spec, or from traffic,
	@# towards evidence; none of them can say that a route which was
	@# there yesterday is there today. check_route_coverage comes
	@# closest and stores `registered` as a COUNT, so deleting a served
	@# route reads in review as `174 -> 173` and never names itself.
	@# Below HTTP nothing looked at all: 28 CLI flags, 2 subcommands and
	@# 37 FABRIC_* knobs that every compose file, CI job and README
	@# snippet here is written against, and a rename would break all of
	@# them while every check above stayed green. Offline like its
	@# neighbour -- the Go source and nothing else -- so it answers
	@# before a push. Asymmetric on purpose: an addition is a stale
	@# ledger you fix with --update, a removal fails until someone
	@# writes down the release it went away in and why.
	@$(PY) scripts/check_backward_compat.py --strict
	@# Doc 24 summarises the sub-plans in one row each, and three of those
	@# rows went stale before anything checked them — each pointing a
	@# maintainer at work already finished.
	@$(PY) scripts/check_plan_freshness.py --strict
	@$(PY) scripts/check_arch_services.py
	@$(PY) scripts/check_refusal_expectations.py
	@$(PY) scripts/check_runtime_floor_freshness.py
	@$(PY) scripts/check_endpoint_env_names.py
	@$(PY) scripts/check_mlflow_unpublished.py
	@$(PY) scripts/check_fabric_activity_types.py
	@$(PY) scripts/check_adf_activity_types.py
	@$(PY) scripts/check_docs_sidebar.py
	@# Prose is the one artifact here with nothing underneath it: a rename
	@# breaks a Go import loudly and the same rename in a sentence breaks
	@# nothing at all. Three had already drifted when this landed.
	@$(PY) scripts/check_doc_drift.py --strict
	@# The same decay one directory over: a Go comment is where the
	@# reasoning lives, and no checker read one until this landed. #490
	@# retired the connector-leaf justification and two comments went on
	@# teaching it in the present tense for a week.
	@$(PY) scripts/check_comment_drift.py --strict
	@$(PY) scripts/check_workflow_concurrency.py
	@# The dependency scanners still watch the repository that is here.
	@# This repo runs gitleaks, govulncheck and Dependabot across five
	@# ecosystems; nothing asked whether that configuration still MATCHES
	@# the tree, and dependabot.yml's own comments record it failing twice
	@# -- seven example lockfiles watched by nothing, eleven of twelve
	@# Dockerfiles unwatched including two published to GHCR. A scanner
	@# that has stopped matching the tree reports clean on exactly the
	@# manifests nobody is watching, which is worse than no scanner
	@# because it produces a green check.
	@$(PY) scripts/check_dependency_risk.py --strict
	@$(PY) scripts/check_cron_workflow_freshness.py
	@$(PY) scripts/gen_event_kinds.py --check
	@$(PY) scripts/check_capture_redaction.py
	@$(PY) scripts/check_entra_install.py
	@# Every sleep in a Go test is inside a bounded loop, or is recorded
	@# in docs/test-flakiness.json with the reason it is accepted. An
	@# unbounded sleep before an assertion makes the verdict a function
	@# of machine load: it passes on a laptop because the thing under
	@# test finished, and on a loaded runner because it had not started.
	@$(PY) scripts/check_test_flakiness.py --strict
	@# The same ban one language over, and it needed its own pass rather
	@# than a widened checker: the Go one counts braces to find a loop's
	@# extent, while Python ships a parser, so the bound shapes here are
	@# decided on the syntax tree. docs/60 recorded the pytest and e2e
	@# suites as unanalysed; 104 sleep sites across them were inspected by
	@# nothing, 5 were genuinely unbounded, and 3 of those were real.
	@$(PY) scripts/check_python_test_flakiness.py --strict
	@# THE SAME BAN, A THIRD LANGUAGE OVER: docs/60-test-flakiness.md's own
	@# first bullet named the portal's vitest suite as the toolchain neither
	@# checker above can see. A regex scan again, like the Go side, since a
	@# TypeScript parser is a third-party import this repo's guards do not
	@# take -- and it needs no Node or pnpm to run, only the source text, so
	@# it belongs here rather than in portal-types below. Measured when this
	@# landed: one real-clock setTimeout across 24 test files, asserting a
	@# negative; rewritten onto src/testing.ts staysAbsent rather than
	@# recorded, so the ledger it checks against ships empty.
	@$(PY) scripts/check_vitest_test_flakiness.py --strict
	@# ...and one level in from all three of them: every script in THIS
	@# directory has a dedicated python/tests/test_<stem>.py, or is
	@# recorded with the reason it does not. These scripts ARE the
	@# invariant enforcement -- the thirty-two lines above -- and a
	@# guard whose own behaviour nothing asserts can stop guarding and
	@# go on reporting green. That is the failure
	@# python/tests/test_make_check_runs_in_ci.py was written about one
	@# level up, in its own words: a check that passes is
	@# indistinguishable from a check that is running. A missing test
	@# module is invisible in review for the same reason -- there is no
	@# diff to notice, only a file that is not there. Measured when this
	@# landed: 11 of 51 scripts, govern_ingest.py at 717 lines the
	@# largest; 3 were closed with real tests and 8 recorded.
	@$(PY) scripts/check_script_test_coverage.py --strict
	@# ...and the one dimension none of the guards above can see at all:
	@# COST. `func Benchmark` returns 0 hits across 552 test files and there
	@# is no stored timing baseline anywhere in this tree, so no performance
	@# regression here is detectable by measurement -- not by this target,
	@# not by CI, not by anything. What IS decidable statically is the shape
	@# that needs no stopwatch to be a regression: per-request work whose
	@# size the CALLER chooses and nothing bounds.
	@#
	@# Measured when this landed: 70 sites consume an inbound request body
	@# (68 `json.NewDecoder(r.Body)`, 2 bare `io.ReadAll(r.Body)`) against 0
	@# uses of `http.MaxBytesReader` in the entire repository. The 23
	@# `httpx.ReadBounded` sites were already bounded, so the tree was
	@# half-guarded in a way that read as fully guarded: every site reading
	@# a body as BYTES went through a ceiling and every site streaming it
	@# through a DECODER went through none. Both bare reads were repaired
	@# and one outer bound now sits at the root handler, which is why the
	@# ledger ships empty rather than 70 entries deep.
	@#
	@# It also holds the ORDERING, which is the half a reader would not
	@# think to check: ReadBounded probes max+1, so an outer bound set AT an
	@# inner ceiling replaces httpx's specific fit-vs-truncated message with
	@# net/http's generic refusal. A 256 MiB outer bound -- equal to
	@# MaxBlobWrite, the ceiling `fab cp` has actually crossed -- would have
	@# shipped that defect looking correct.
	@$(PY) scripts/check_perf_regressions.py --strict
	@# ...and the dimension none of the guards above can see either: whether
	@# this repository's own source is WRITTEN in a shape that leaks a
	@# credential or drops a trust check. Measured when this landed:
	@# .golangci.yml enables [errcheck, govet, ineffassign, staticcheck,
	@# unused] and NO gosec, so the Go linter carries no security analyser at
	@# all; ruff's flake8-bandit family is not in pyproject.toml's `select`,
	@# though fifteen `# noqa: S###` directives in the tree were written as
	@# though it were -- suppressions for a check nobody ran. The two
	@# scanners that DO run answer different questions: govulncheck watches
	@# DEPENDENCIES (a vulnerable symbol this code can reach) and gitleaks
	@# watches COMMITTED STRINGS (a secret in the pack). Neither reads this
	@# source for code shape, and SECURITY.md's "what does not run" section
	@# could not name the gap because the gap was the whole category.
	@#
	@# The rule set is SECURITY.md's IN-SCOPE list, not a generic scanner's,
	@# because this emulator is deliberately insecure in documented ways --
	@# seeded secrets, self-signed TLS, an unauthenticated admin API -- and a
	@# checker that reported those would be argued with rather than fixed.
	@# So the emulator-only sites are LEDGERED with the host set each client
	@# reaches (docs/security-footguns.json), and 30 are. Two rules ship at
	@# zero as pure regression guards; the first run found one real defect --
	@# internal/akv disabled certificate verification transport-wide from a
	@# flag documented for entra-emulator's cert, on the one client whose
	@# allowlist deliberately admits a real *.vault.azure.net, so a secret
	@# could come from a real vault over a connection nobody authenticated
	@# while the https check that exists for that reason still passed.
	@$(PY) scripts/check_security_footguns.py --strict

# Not part of `check`: these need Node and an installed portal, and `check` is
# deliberately runnable with nothing but Python. CI runs both in the portal-types
# job.
portal-types: ## Type-check the portal, and prove a new event kind breaks it
	pnpm --filter fabric-emulator-portal check
	@$(PY) scripts/check_kind_exhaustiveness.py

test: check ## Repo invariants, then Go build, vet and unit tests
	go build ./... && go vet ./... && go test ./...

# NOT folded into `test:`. The race detector costs roughly 5x the wall time of a
# plain run (measured on this tree: 2m04s against 25s), and `test` is the target
# someone types in a tight edit loop -- a gate that slow gets run less, not more.
# CI runs this one on every push and pull request (.github/workflows/ci.yml,
# the `race` job), which is where the cost belongs.
#
# WHY IT EXISTS AT ALL. Before this, `grep -rn -- '-race' .github/workflows/
# Makefile scripts/` returned NOTHING, and so did the same grep for `-shuffle`:
# the race detector had never run against this repository, across 285 test files
# with 68 goroutine-spawning sites in them and a real event-bus dispatcher
# delivering to subscribers on its own goroutine. The suite turned out to be
# clean -- see docs/60-test-flakiness.md for the measured sweep -- which is the
# good outcome and not the point. An unrun detector reports the same silence
# whether or not there is anything to find.
#
# Both flags together, deliberately. `-race` finds unsynchronised access;
# `-shuffle=on` finds the other half, a test that passes only because an earlier
# test in the same file left state behind. Neither sees what the other does.
test-race: ## The race detector and randomised test order over the whole Go suite
	go test -race -shuffle=on -count=1 ./...

# ---------------------------------------------------------------------------
# The documentation site.
#
# Not called `docs`: there is a docs/ DIRECTORY here, and a target sharing its
# name is satisfied by the directory existing. `make docs` would print
# "nothing to be done" and exit 0, which is the failure that looks like
# success. .PHONY below would also fix it; a name that cannot collide fixes it
# whether or not someone remembers .PHONY.
#
# `pnpm --filter $(DOCS_PKG) dev` is the fast inner loop for PROSE, and it is
# not this. It is based at the docs subpath and knows nothing about the tree
# around it, so under it the landing page does not exist, the redirect stubs do
# not exist, and the badge documents the landing page fetches do not exist. Use
# it to write a page; use `make docs-serve` before believing the site works.
#
# CI runs `make docs-build` and publishes what it leaves in ./_site, with ONE
# addition it cannot make here: the coverage badges, whose numbers come from
# the last CI run's artifact and are not reproducible locally. This target
# writes them as "n/a" instead, which is the same fallback the workflow uses
# when no artifact is found, so the tiles are laid out the way they publish.
DOCS_PKG  ?= fabric-emulator-docs
DOCS_PORT ?= 8099
# The interpreter CI uses, pinned. These scripts are stdlib-only, hence
# --no-project: no environment to resolve, and a local 3.9 cannot pass
# something 3.12 would reject.
UVPY ?= uv run --no-project --python 3.12 python

docs-build: ## Build the published site into ./_site (what CI deploys)
	@command -v uv >/dev/null 2>&1 || { echo "uv is not on PATH: https://docs.astral.sh/uv/" >&2; exit 1; }
	pnpm install --frozen-lockfile
	$(UVPY) scripts/check_docs_links.py --strict
	pnpm --filter $(DOCS_PKG) build
	$(UVPY) scripts/assemble_site.py --self-test
	$(UVPY) scripts/assemble_site.py --out _site
	@# The n/a badges named above, written where CI copies the real ones over
	@# them: both at the root, where the README's published shields URLs name
	@# them, and under /docs/. AFTER the assembler, which clears _site.
	$(UVPY) scripts/coverage_badges.py --out _site
	$(UVPY) scripts/coverage_badges.py --out _site/docs
	$(UVPY) scripts/build_landing_data.py --out _site --landing website/src/pages/index.astro

docs-serve: docs-build ## …and serve it locally at its published URLs (DOCS_PORT=8099)
	$(UVPY) scripts/assemble_site.py --serve --site _site --port $(DOCS_PORT)
