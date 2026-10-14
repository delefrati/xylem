SHELL := /bin/bash
.DEFAULT_GOAL := help

BIN := $(CURDIR)/bin
# Pinned tools (and protoc plugins, for buf) come first.
export PATH := $(BIN):$(PATH)

COMPOSE  := docker compose -f deploy/docker-compose.yml
VERSION  := $(shell git describe --always --dirty 2>/dev/null || echo dev)
COMMIT   := $(shell git rev-parse --short HEAD 2>/dev/null)
LDFLAGS  := -X main.version=$(VERSION) -X main.commit=$(COMMIT)
FUZZTIME ?= 30s
GOTESTFLAGS ?=

include tools.mk

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

## ---- local services

.PHONY: up down reset-db
up: ## Start postgres + minio and wait until healthy and the bucket exists
	$(COMPOSE) up -d --wait postgres minio
	$(COMPOSE) run --rm --quiet-pull minio-init

down: ## Stop local services (keeps volumes)
	$(COMPOSE) down

reset-db: build ## Drop and recreate the xylem DB, then run migrations
	$(COMPOSE) exec -T postgres psql -U xylem -d postgres -v ON_ERROR_STOP=1 \
		-c 'DROP DATABASE IF EXISTS xylem WITH (FORCE)' -c 'CREATE DATABASE xylem'
	$(BIN)/xylemd migrate

## ---- code generation and build

.PHONY: proto build
proto: ## buf lint + buf generate into gen/
	buf lint
	buf generate

build: ## Build bin/xy and bin/xylemd
	go build -ldflags "$(LDFLAGS)" -o $(BIN)/ ./cmd/...

## ---- quality

.PHONY: lint test test-integration check fuzz bench
lint: ## golangci-lint + buf lint
	golangci-lint run
	buf lint

test: ## Unit tests (no Docker)
	go test $(GOTESTFLAGS) ./...

test-integration: ## Unit + integration tests (needs make up)
	go test $(GOTESTFLAGS) -tags integration ./...

check: ## lint + unit + integration with -race + build (needs make up)
	$(MAKE) lint
	$(MAKE) test-integration GOTESTFLAGS=-race
	$(MAKE) build

fuzz: ## Run every Fuzz* target for FUZZTIME (default 30s)
	@set -e; for pkg in $$(go list ./...); do \
		for fn in $$(go test -list '^Fuzz' $$pkg | grep '^Fuzz' || true); do \
			echo "== $$pkg $$fn"; \
			go test -run '^$$' -fuzz "^$$fn$$" -fuzztime $(FUZZTIME) $$pkg; \
		done; \
	done

bench: ## Go benchmarks; integration ones too when services are up
	@tags=; if [ -n "$$($(COMPOSE) ps -q --status running postgres 2>/dev/null)" ]; then tags="-tags integration"; fi; \
	echo "go test $$tags -run=^\$$ -bench=. -benchmem ./..."; \
	go test $$tags -run='^$$' -bench=. -benchmem ./...

## ---- later milestones

.PHONY: parity parity-external e2e fixtures run-server demo
parity: ## Git parity suite: import each fixture, serve it in process, compare every check with git
	go test -tags parity -count=1 -timeout 20m -v -run TestParity ./internal/parity/...

parity-external: ## Git parity suite on testdata/external (M4)
	@echo "parity-external: not yet implemented (M4)"

e2e: ## CLI end-to-end tests (M5)
	@echo "e2e: not yet implemented (M5)"

FIXTURES := testdata/fixtures

fixtures: ## Generate the tiny, edge and branchy fixture repos into testdata/fixtures
	scripts/make-fixture-repo.sh $(FIXTURES)/tiny tiny
	scripts/make-fixture-repo.sh $(FIXTURES)/edge edge
	go run ./scripts/genrepo $(FIXTURES)/branchy

run-server: ## Run bin/xylemd server with dev settings (M4)
	@echo "run-server: not yet implemented (M4)"

demo: ## Full local demo (M6)
	@echo "demo: not yet implemented (M6)"
