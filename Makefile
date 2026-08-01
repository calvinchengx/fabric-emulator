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
PROFILE ?= --profile governance
COMPOSE  = docker compose $(PROFILE)

# Windows: force the recipes onto sh.exe. GNU Make on Windows falls back to
# cmd.exe when it cannot find a shell, and cmd cannot run a single line of what
# is below. Make searches PATH for this itself, so the spaces in
# "C:\Program Files\Git\bin" are its problem, not ours.
ifeq ($(OS),Windows_NT)
  SHELL := sh.exe
  .SHELLFLAGS := -c
endif

# Which interpreter is "python3" is not a given. On Windows `python3` normally
# resolves to the Microsoft Store *alias stub*: it exists on PATH, so
# `command -v python3` succeeds, and then it exits 49 with a "not found, install
# from the Store" message. Detection therefore has to RUN each candidate, not
# merely locate it — the same reason scripts/status.sh no longer trusts
# `command -v`. Override with PY= if you keep python somewhere unusual.
PY ?= $(shell for c in python3 python py; do if "$$c" -c '' >/dev/null 2>&1; then echo "$$c"; break; fi; done)

.PHONY: help doctor up down restart clean status status-spark spark logs ps seed test

help: ## Show the available targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  %-14s %s\n", $$1, $$2}'

doctor: ## Check the toolchain and the docker context this Makefile needs
	@sh scripts/doctor.sh

up: ## Start the whole stack in the background
	$(COMPOSE) up -d

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
	@test -n "$(PY)" || { echo "no working python found (tried python3, python, py); set PY=" >&2; exit 1; }
	$(PY) scripts/spark_check.py

seed: ## Catalog the emulator into OpenMetadata (seeds a demo if empty)
	$(COMPOSE) run --rm govern-ingest

ps: ## Container states for this project
	$(COMPOSE) ps

logs: ## Tail logs (SVC=<service> to narrow)
	$(COMPOSE) logs -f --tail 100 $(SVC)

test: ## Go build, vet and unit tests
	go build ./... && go vet ./... && go test ./...
