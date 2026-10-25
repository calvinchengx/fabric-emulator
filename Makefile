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

sbom: ## Write a CycloneDX SBOM of every locked dependency to sbom.cdx.json
	@$(PY) scripts/gen_sbom.py --out sbom.cdx.json

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
	@# The portal's ui/ is VENDORED -- shadcn-svelte components copied in
	@# by a generator, not resolved from a package -- so nothing in the
	@# dependency graph has an opinion about whether one is still used. An
	@# unimported component compiles, type-checks, passes vitest and ships
	@# forever, because the failure is an import that is NOT there and there
	@# is no diff to notice. Measured when this landed: select/ (12 files)
	@# was reachable from nothing, and separator/ (2 files) had exactly one
	@# referrer repo-wide -- select/select-separator.svelte. So separator/
	@# was held alive by code that was itself dead, which is why the
	@# assertion is reachability from the APPLICATION rather than
	@# mention-anywhere: a referrer inside ui/ does not count, and the two
	@# fall out of one question together. DIRECTORY granularity on purpose,
	@# since a barrel publishes a component's whole surface and a
	@# file-level sweep would flag card-action and table-footer, which are
	@# not defects. Offline and stdlib-only -- it reads portal SOURCES, not
	@# the built bundle -- so it belongs here rather than in portal-types
	@# below, which needs Node and an installed portal.
	@$(PY) scripts/check_portal_components.py --strict
	@# ...and the same question one language over, where nothing was asking
	@# it at all. ruff runs over every line of Python here and its `F` family
	@# is narrower than people assume: F401 is an unused IMPORT, F841 an
	@# unused LOCAL. Neither has an opinion about a module-level def, class
	@# or constant that nothing reads, and the vulture-style rule that would
	@# is not a ruff rule and is not in pyproject.toml's `select`. Go was
	@# already covered -- .golangci.yml enables `unused`, and a scan of every
	@# exported Go identifier found zero orphans because that linter has held
	@# the line the whole time -- and the portal is covered by the gate above
	@# plus svelte-check. Python module scope was the one surface with nobody
	@# asking.
	@#
	@# Measured when this landed: 1538 module-level symbols across 107
	@# in-scope files, of which THREE were reached by nothing.
	@# usercontext._KEEP is the one worth reading: a tuple of environment
	@# names under a comment describing an ALLOW list, in a module whose
	@# child_env pops CREDENTIAL_ENV from a copy -- a DENY list, which its own
	@# docstring spends a paragraph arguing for. So the comment documented
	@# behaviour the code never had, which outlives the dead tuple it sat on.
	@# The other two were check_security_footguns._SSL_OFF_ATTRS, superseded
	@# by _py_ssl_skips, and contoso-fixtures ORDER_FIELDS, a second and
	@# silently drifting statement of the bronze_orders schema the generator
	@# writes as a dict literal 150 lines below it.
	@#
	@# MENTION-ANYWHERE, generously and on purpose: a Makefile recipe, a CI
	@# job, a docstring or a doc all count as a reference, so a name this
	@# reports is spelled NOWHERE else in the tree and the finding is never
	@# arguable. It is the weaker reading and it is the right one -- a guard
	@# whose output gets argued with instead of fixed stops being run, which
	@# is what check_perf_regressions.py's `defer-in-loop` records.
	@#
	@# The exemptions carry their own measured cost. test_*/conftest.py is
	@# the load-bearing one at 2190 would-be findings, because pytest
	@# discovers and injects BY NAME; __init__ barrels, decorated defs,
	@# dunders, `main` and `__all__` names are at zero today and are stated
	@# as such rather than implied to be doing work. `build/` is the one that
	@# would have silently disabled the check over a whole package:
	@# python/fabric-target/build/lib/ is a TRACKED copy of the source, so
	@# every symbol in it appeared twice and none could ever be reported.
	@#
	@# AND IT SHIPPED THE DEFECT IT EXISTS TO CATCH, which is why the guard's
	@# own prose is excluded from the counting pass. The house style requires
	@# a checker to name its measured baseline, so this one's docstring spells
	@# all three symbols -- and under mention-anywhere that paragraph IS a
	@# second occurrence of each. Its first run over the unrepaired tree
	@# reported 0 orphans and exit 0, having laundered exactly what it was
	@# written to find. The file's CODE still counts (the repair's own first
	@# version skipped it outright and then reported nine of its internal
	@# helpers as orphans), so it is counted from its NAME tokens alone.
	@$(PY) scripts/check_dead_python_symbols.py --strict
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
	@# Below HTTP nothing looked at all: 30 CLI flags, 2 subcommands and
	@# 41 FABRIC_* knobs that every compose file, CI job and README
	@# snippet here is written against, and a rename would break all of
	@# them while every check above stayed green. The fifth surface it
	@# holds is served over no wire: the 51 exported ids of `pkg/`,
	@# which is a COMPILE-TIME contract -- that directory exists so
	@# another module CAN import it, where Go's internal rule would
	@# forbid it, so dropping `AccessEntry.Why` breaks a downstream
	@# BUILD while every gate here stays green. Offline like its
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
	@#
	@# Invariants 6 and 7 turn that question on the manifests themselves,
	@# which the five above cannot reach. This repository can decide to
	@# live with a known risk in four places and only three had any
	@# bookkeeping: a dependabot `ignore:` (invariant 3), a dismissed
	@# alert (check_dismissed_advisories.py) and an advisory-holds.json
	@# record (invariant 5). The fourth is an `==` in a pyproject.toml --
	@# the STRONGEST hold available and the only one with none, an
	@# `ignore:` with no entry, no comment and no review -- and a grep for
	@# the two exit markers across all eleven tracked pyproject.toml files
	@# returned 0 against 8 exact pins. Seven were policy (the
	@# pyspark-client/pysail versions are one number, a fidelity claim
	@# about which Spark the shipped engine IS); one was not. Invariant 7
	@# then asks whether a constraint an example SHARES with the root
	@# agrees with it, which matters because examples are what people copy
	@# out: both advanced medallions pinned `great-expectations<1` under a
	@# comment asserting they could not drift from the root, while the
	@# root had already moved to `<2` and resolves 1.23.2 against their
	@# 0.18.22. Both invariants read COMMENT RUNS, so
	@# check_comment_drift.py --strict above reads these too.
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
	@# A FOURTH CHECKER RATHER THAN A WIDENED ONE, and on a different AXIS
	@# from the three above rather than a fourth language. All three ask
	@# whether a WAIT is bounded; docs/60-test-flakiness.md's own §6 named
	@# what that leaves out -- "flakiness with no timing tell ... nothing
	@# here covers within-test nondeterminism that the race detector cannot
	@# see." This flags one sharply-defined member of that class: a
	@# wall-clock read truncated to a DAY in test or e2e-harness code.
	@#
	@# Measured when this landed: 3 Go sites and 8 Python ones, of which 4
	@# were LOAD-BEARING and 2 of those were separate defects. A test
	@# performed operations whose audit events were stamped from the store's
	@# clock, THEN read the wall clock a second time to compute "today", then
	@# asserted those events appear in that day's window -- so a run crossing
	@# UTC midnight between the two reads queries a day the events are not in
	@# and fails on a missing CreateWorkspace. e2e/az-rest/driver.py had the
	@# identical shape one surface over. No sleep to flag, no shared memory
	@# for -race, no cross-test ordering for -shuffle=on: the whole failure is
	@# two readings of one clock either side of a boundary, once a day, in a
	@# sub-second window. Two more were found during the repair -- labels_test
	@# and the pagination test reach the same window through a helper, so the
	@# grep that measured the sweep could not see them.
	@#
	@# The repair is NOT a widened window: the API refuses bounds spanning two
	@# days, and TestActivityEventsRequestValidation's `spans two days` case
	@# pins that 400. So the day is captured ONCE before the mutations and the
	@# straddle is DETECTED and unioned with the following day's single-day
	@# query (internal/testsupport.UTCDaysSince, and activity_days in the
	@# driver, which cannot import it -- it runs inside the azure-cli image).
	@# What the checker can see is that a day was read; whether the straddle
	@# is handled is held by internal/testsupport/clock_test.go instead, and
	@# that split is written down rather than left implied.
	@$(PY) scripts/check_clock_test_flakiness.py --strict
	@# ...and one level in from all four of them: every script in THIS
	@# directory has a dedicated python/tests/test_<stem>.py, or is
	@# recorded with the reason it does not. These scripts ARE the
	@# invariant enforcement -- the thirty-eight lines above -- and a
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
	@#
	@# And the SECOND LISTENER, which all of the above is blind to by
	@# construction: it anchors on `r.Body`, and cmd/fabric-emulator/main.go
	@# serves the SQL/TDS endpoint on its own net.Listener where there is no
	@# `r.Body` for an outer bound to wrap. tds.ReadMessage joined packets
	@# until one set the EOM status bit with no cap on the total -- each
	@# ReadPacket is self-limiting at 64 KiB because the length field is a
	@# uint16, which is exactly why the loop around it read as bounded and
	@# was not. Measured when this landed: 1 http.Serve site in the tree, 1
	@# TDS listener beside it, and 0 accumulated-length checks across all 40
	@# files of internal/tds -- against 8 TDS read sites, all funnelling
	@# through ReadMessage, so one ceiling there covers the whole listener.
	@# Repaired by tds.DefaultMaxMessageBytes (320 MiB, the HTTP listener's
	@# own default, so neither listener is the narrower surface), behind
	@# FABRIC_MAX_TDS_MESSAGE_BYTES with 0 meaning unlimited for the same
	@# released-binary reason the HTTP knob has one -- and the direction it
	@# narrows is the RELAY one, since splice.go reads the sidecar's result
	@# sets through the same function.
	@#
	@# Held by two more kinds. The first asserts the whole CHAIN the bound
	@# travels -- declared, derived from the default, routed by ReadMessage,
	@# and COMPARED against in the function that joins, before the append
	@# that would cross it -- because its own first version checked only
	@# that ReadMessage named the constant, which a one-line delegation to a
	@# helper satisfies while the helper compares against nothing. The
	@# second requires every serve site to be handed Server.Handler(), so a
	@# future second HTTP listener handed the bare mux lands as a finding
	@# rather than as a silent bypass.
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
	@# reaches (docs/security-footguns.json), and 85 are. Three rules ship at
	@# zero as pure regression guards; the first run found one real defect --
	@# internal/akv disabled certificate verification transport-wide from a
	@# flag documented for entra-emulator's cert, on the one client whose
	@# allowlist deliberately admits a real *.vault.azure.net, so a secret
	@# could come from a real vault over a connection nobody authenticated
	@# while the https check that exists for that reason still passed.
	@#
	@# AND THEN THE SAME DEFECT SIX MORE TIMES, from one spelling the TLS rule
	@# had never learned. R1 claimed to cover every certificate-verification
	@# skip in this tree and matched requests' `verify=False` only -- not the
	@# stdlib pair `verify_mode = ssl.CERT_NONE` / `check_hostname = False`,
	@# which is what this repository's OWN Python writes, in the spark agent,
	@# in fabric-target, in the Sail launcher and across 28 e2e harnesses. 39
	@# sites were unexamined; six carried a credential over the unverified
	@# connection, and eventstream_kafka._fabric_request sent a bearer token
	@# to a FABRIC_API_URL DEFAULTING to api.fabric.microsoft.com -- so the
	@# default configuration reached a real Microsoft endpoint with the trust
	@# check off. Four more POST ENTRA_CLIENT_SECRET. All six are now scoped
	@# per host (python/spark_agent/tls_scope.py and fabric_target's
	@# _tls_context, both citing akv.hostScopedTLS) rather than recorded.
	@#
	@# The seventh rule is the host-escape shape R5/R6 did not cover: an
	@# archive member extracted with nothing checking where it lands.
	@# filepath.Join CLEANS its result, so Join("/out", "../../etc/x") is a
	@# perfectly clean /etc/x -- which is why the shape reads as safe -- and
	@# tarfile's filter="data" only becomes the default in 3.14 against this
	@# repo's 3.12 floor. e2e/azcopy/run.py unpacked an aka.ms download with
	@# bare extractall in both branches; both were repaired, so the rule
	@# ships empty. Its Go half has never had a finding because no non-test
	@# Go file here imports archive/zip or archive/tar, and the docstring
	@# says so rather than implying a measurement.
	@#
	@# AND AN EIGHTH, whose asymmetry runs the OTHER way: a name spliced into
	@# SQL text is SQL SYNTAX, not a value, and backticks around it look like
	@# quoting until the name contains one. Measured when this landed, every
	@# GO site in this tree already escaped correctly -- principalName doubles
	@# `]`, quotedID doubles `'`, internal/server's sqlIdent/sqlLiteral are
	@# the same pair, enterDelegated leans on SQL Server's own QUOTENAME --
	@# while NOT ONE backtick was doubled anywhere in python/spark_agent,
	@# onelake_security.apply included. That one builds the temp view which
	@# ENFORCES OneLake row- and column-level security, from a name that is a
	@# OneLake folder created by whoever can write there, so a breakout picks
	@# the body of the view deciding which rows a principal sees rather than
	@# raising a syntax error. Eight symbols repaired onto sqlident's
	@# quote_ident/quote_literal/quote_name (the Go pair, mirrored rather than
	@# half of it); the six correct Go symbols are LEDGERED as regression guards,
	@# because that half of the rule reads the enclosing function and cannot
	@# see an escape in the argument list where the Python half can.
	@#
	@# The Python half recognising the escape is what makes the repair a
	@# RATCHET rather than twenty more ledger entries: an escaped site reports
	@# clean, so deleting a quote_ident call becomes an unrecorded finding and
	@# exit 1. Two of the eight the rule CANNOT see -- secure_view_sql returns
	@# SQL instead of running it, _drop binds its statements to a variable
	@# first -- and both were fixed anyway, since they are the column-only and
	@# the table-withholding routes of the same enforcement.
	@#
	@# A THIRD WAS INVISIBLE TO R8 ITSELF, and it is the sharpest of the eight:
	@# delta_ops.execute_ctas is reached as original_sql(...), a bare NAME that
	@# delta_ops binds to spark.sql and passes as a parameter, so the rule's
	@# METHOD requirement -- right about `execute` being a name anything may
	@# have -- walked past it. It spliced a catalog-supplied schema location
	@# into LOCATION '{loc}' with nothing doubling an apostrophe, inside the one
	@# package R8's scope sentence claimed to have swept, while catalog.register
	@# escaped the SAME value one module over. The rule now accepts a bare call
	@# to a name THIS FILE binds to a .sql/.execute attribute, so that repair
	@# ratchets too.
	@#
	@# Ordinary names come back byte-identical, which is the only reason a
	@# change touching every statement the agent builds moved no fixture or
	@# e2e output.
	@$(PY) scripts/check_security_footguns.py --strict
	@# ...and the dimension the three above cannot see either: whether this
	@# emulator says anything at all when it fails. Measured when this landed:
	@# 30 `log.*` call sites across 12 non-test Go files carry the WHOLE of its
	@# account of its own failure paths -- a Livy session whose lakehouse did
	@# not mount, a notebook driver that panicked, a recording file CI is about
	@# to upload that could not be opened -- and nothing registered them. There
	@# is no metrics endpoint and no structured sink; in a compose stack this is
	@# the evidence.
	@#
	@# The failure shape is this repo's own, turned inside out. A check that
	@# passes is indistinguishable from a check that is running
	@# (test_make_check_runs_in_ci.py); a handler that stops logging its failure
	@# path is indistinguishable from a handler that has no failures. What is
	@# missing is a line that is not there, so there is no diff to notice.
	@#
	@# Four conventions for the subsystem tag coexisted -- bracketed
	@# `[onelake-dfs]`, colon `livy:`, space-delimited `notebook drive job=`,
	@# and three lines with no tag at all -- so NO single grep selected one
	@# subsystem out of a compose log. All 30 now carry one of 13 ledgered tags.
	@#
	@# And the finding that started it: ONELAKE_TRACE was the only log-gating
	@# knob not named FABRIC_*, which is how it escaped the surface ledger
	@# entirely. check_backward_compat.py above finds knobs by scanning for that
	@# literal, so this one had no envVars row, no docsUndocumented reason and
	@# no mention in docs/ -- a knob released binaries honour that every gate
	@# here was blind to, while its sibling FABRIC_TDS_TRACE carried both. It is
	@# FABRIC_ONELAKE_TRACE now, the legacy spelling is still read and recorded,
	@# and R3 is what stops the next one landing outside.
	@$(PY) scripts/check_logging_quality.py --strict
	@$(PY) scripts/check_port_guard.py

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
