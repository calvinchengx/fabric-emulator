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

.PHONY: help doctor up up-lite up-jupyter up-jvm up-eventstream dax-linux down restart clean status status-spark spark logs ps seed test check lint docs-build docs-serve

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
	@$(PY) scripts/check_arch_services.py
	@$(PY) scripts/check_endpoint_env_names.py
	@$(PY) scripts/check_mlflow_unpublished.py
	@$(PY) scripts/check_fabric_activity_types.py
	@$(PY) scripts/check_adf_activity_types.py
	@$(PY) scripts/check_docs_sidebar.py
	@$(PY) scripts/check_workflow_concurrency.py
	@$(PY) scripts/check_cron_workflow_freshness.py
	@$(PY) scripts/gen_event_kinds.py --check
	@$(PY) scripts/check_capture_redaction.py
	@$(PY) scripts/check_entra_install.py

# Not part of `check`: these need Node and an installed portal, and `check` is
# deliberately runnable with nothing but Python. CI runs both in the portal-types
# job.
portal-types: ## Type-check the portal, and prove a new event kind breaks it
	pnpm --filter fabric-emulator-portal check
	@$(PY) scripts/check_kind_exhaustiveness.py

test: check ## Repo invariants, then Go build, vet and unit tests
	go build ./... && go vet ./... && go test ./...

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
