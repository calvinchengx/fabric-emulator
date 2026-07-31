# Thin wrappers over the docker compose + scripts workflow. The compose files
# remain the source of truth; this exists so the everyday cycle is one word
# each. Nothing here is required — every target shows the command it runs.
#
#   make up      # start the stack (governance profile: OpenMetadata + ingest)
#   make status  # is the stack actually usable? (exit non-zero if not)
#   make clean   # stop everything AND delete the data volumes
#
# The governance profile is on by default so `make up` matches what the
# quickstart advertises; override with PROFILE= to run the lean stack.
PROFILE ?= --profile governance
COMPOSE  = docker compose $(PROFILE)

.PHONY: help up down restart clean status status-spark spark logs ps seed test

help: ## Show the available targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  %-14s %s\n", $$1, $$2}'

up: ## Start the whole stack in the background
	$(COMPOSE) up -d

down: ## Stop and remove containers (volumes SURVIVE)
	$(COMPOSE) down

clean: ## Stop and remove containers AND delete the data volumes (full reset)
	$(COMPOSE) down -v

restart: clean up ## Full reset: clean, then start again

status: ## Report whether the stack is usable (non-zero exit if not)
	@./scripts/status.sh

status-spark: ## status, plus a real Livy session executing Spark statements
	@./scripts/status.sh --spark

spark: ## Deep Spark check only (Livy -> spark-agent -> sail)
	@python3 scripts/spark_check.py

seed: ## Catalog the emulator into OpenMetadata (seeds a demo if empty)
	$(COMPOSE) run --rm govern-ingest

ps: ## Container states for this project
	$(COMPOSE) ps

logs: ## Tail logs (SVC=<service> to narrow)
	$(COMPOSE) logs -f --tail 100 $(SVC)

test: ## Go build, vet and unit tests
	go build ./... && go vet ./... && go test ./...
