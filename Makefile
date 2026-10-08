.DEFAULT_GOAL := help

ENV_FILE ?= .env
GO       ?= go
DB_CONTAINER ?= ccs-postgres
DB_IMAGE     ?= postgres:16-alpine
DB_PORT      ?= 5432
LOG_DIR      ?= logs
PW_SCRAPE_LOG ?= $(LOG_DIR)/pw-scrape.log
PW_SCRAPE_OUTPUT ?= $(LOG_DIR)/pw-batches.json
PW_SCRAPE_RAW_OUTPUT ?= $(LOG_DIR)/pw-batches-raw.json

# Pull in .env if present so `make run-api` etc. pick up DATABASE_URL, PABBLY_*, ...
ifneq (,$(wildcard $(ENV_FILE)))
	include $(ENV_FILE)
	export
endif

.PHONY: help
help: ## Show this help
	@echo "Course Marketplace — available targets:"
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_.-]+:.*##/ {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

## --- Environment -----------------------------------------------------------

.PHONY: env
env: ## Create .env from .env.example if it doesn't exist yet
	@test -f $(ENV_FILE) || cp .env.example $(ENV_FILE)
	@echo "$(ENV_FILE) ready — fill in PABBLY_API_KEY / PABBLY_SECRET_KEY / PABBLY_WEBHOOK_SECRET"

.PHONY: db-up
db-up: ## Start a throwaway local Postgres in Docker (skip if you already have one)
	@docker start $(DB_CONTAINER) 2>/dev/null || docker run -d --name $(DB_CONTAINER) \
		-e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=marketplace \
		-p $(DB_PORT):5432 $(DB_IMAGE)
	@echo "Postgres listening on localhost:$(DB_PORT) (db=marketplace user=postgres pass=postgres)"

.PHONY: db-down
db-down: ## Stop and remove the local Postgres container
	@docker rm -f $(DB_CONTAINER) 2>/dev/null || true

.PHONY: db-shell
db-shell: ## Open a psql shell against DATABASE_URL
	@psql "$(DATABASE_URL)"

## --- Build / quality --------------------------------------------------------

.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy

.PHONY: fmt
fmt: ## gofmt all Go source
	gofmt -w cmd internal migrations

.PHONY: vet
vet: ## go vet everything
	$(GO) vet ./...

.PHONY: build
build: ## Build all three binaries into ./bin
	@mkdir -p bin
	$(GO) build -o bin/api     ./cmd/api
	$(GO) build -o bin/worker  ./cmd/worker
	$(GO) build -o bin/seed    ./cmd/seed
	$(GO) build -o bin/migrate ./cmd/migrate

.PHONY: test
test: ## Run unit tests (provider adapter tests; no DB required)
	$(GO) test ./...

.PHONY: check
check: fmt vet test build ## fmt + vet + test + build, in that order

## --- Running the app --------------------------------------------------------

.PHONY: migrate
migrate: ## Apply pending SQL migrations against DATABASE_URL right now, without starting a server
	$(GO) run ./cmd/migrate

.PHONY: run-api
run-api: logs-dir ## Run the HTTP API (foreground), logging JSON to stdout and to logs/api.log
	$(GO) run ./cmd/api 2>&1 | tee -a $(LOG_DIR)/api.log

.PHONY: run-worker
run-worker: logs-dir ## Run the background worker (catalog sync, expiry, webhooks, coupon cleanup)
	$(GO) run ./cmd/worker 2>&1 | tee -a $(LOG_DIR)/worker.log

.PHONY: seed
seed: ## Seed the dev wallet + default offers (run once after the first catalog sync)
	$(GO) run ./cmd/seed

.PHONY: logs-dir
logs-dir:
	@mkdir -p $(LOG_DIR)

.PHONY: dev
dev: db-up ## Start Postgres, then run api + worker together (Ctrl-C stops both)
	@$(MAKE) -j2 run-api run-worker

## --- Logs / audit ------------------------------------------------------------

.PHONY: scrape-pw-batches
scrape-pw-batches: logs-dir ## Scrape PW batches; save logs and the latest JSON report under logs/
	@if [ -f "$(ENV_FILE)" ]; then \
		echo "--> Loading environment from $(ENV_FILE)" >&2; \
		set -a; . ./$(ENV_FILE); set +a; \
	else \
		echo "--> Warning: $(ENV_FILE) not found, using system environment" >&2; \
	fi; \
	PW_LOG_FILE="$(PW_SCRAPE_LOG)" PW_OUTPUT_FILE="$(PW_SCRAPE_OUTPUT)" PW_RAW_OUTPUT_FILE="$(PW_SCRAPE_RAW_OUTPUT)" $(GO) run ./cmd/worker scrape-pw-batches $(ARGS)

.PHONY: logs-pw
logs-pw: ## Tail PW scraper logs
	@tail -f $(PW_SCRAPE_LOG)

.PHONY: logs-api
logs-api: ## Tail the API's JSON logs
	@tail -f $(LOG_DIR)/api.log

.PHONY: logs-worker
logs-worker: ## Tail the worker's JSON logs
	@tail -f $(LOG_DIR)/worker.log

.PHONY: logs-audit
logs-audit: ## Tail only audit-tagged log lines (coin movements, redemption/coupon status changes) across both logs
	@tail -f $(LOG_DIR)/api.log $(LOG_DIR)/worker.log 2>/dev/null | grep --line-buffered '"audit":true'

.PHONY: clean
clean: ## Remove build artifacts and local logs
	rm -rf bin $(LOG_DIR)
