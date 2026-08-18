# pager — build & test.
#
# The SQLite driver is pure Go (modernc.org/sqlite), so CGO is never required:
# the binary cross-compiles and installs without a C toolchain. CGO_ENABLED=0
# is exported here rather than passed per-target so a stray `go test` in a
# subshell cannot silently link against libsqlite3 instead.
#
# Hook registration and MCP registration land in I10 (install/docs).

SHELL  := /bin/bash
BINDIR ?= $(HOME)/.local/bin
BIN    := $(BINDIR)/pager

export CGO_ENABLED := 0

.PHONY: help build reinstall rollback smoke test vet fmt check clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-10s\033[0m %s\n",$$1,$$2}'

# smoke is the gate a binary passes before it is allowed to serve live hooks.
#
# `--help` does not qualify, which is why it is not what runs here: it never
# opens the store, so a build whose store handling is broken — the failure this
# project has actually shipped, a WAL switch that lost a race on first open —
# sails through it and dies in the first hook instead. `who` opens the database,
# migrates it, and queries it, and exits 0 whether or not any session exists.
#
# The database is a fresh temporary file rather than the real inbox: the gate
# has to exercise create-and-migrate, and it must not depend on the state of an
# inbox that live hooks are writing to at the same moment.
smoke: ## Check that a binary can open its store (CAND=<path>)
	@test -n "$(CAND)" || { echo "smoke needs CAND=<path to a pager binary>" >&2; exit 1; }
	@tmp=$$(mktemp -d); \
	 if ! PAGER_DB="$$tmp/smoke.db" "$(CAND)" who >/dev/null; then \
	   rm -rf "$$tmp"; \
	   echo "✗ smoke test failed: $(CAND) cannot open its store" >&2; \
	   exit 1; \
	 fi; \
	 rm -rf "$$tmp"

build: ## Build pager into ~/.local/bin (replace-while-running safe)
	@mkdir -p "$(BINDIR)"
	go build -o "$(BIN).new" ./cmd/pager
	@$(MAKE) --no-print-directory smoke CAND="$(BIN).new"
	@mv "$(BIN).new" "$(BIN)"
	@echo "✓ installed $(BIN)"

# Upgrading in place is the normal case: hooks in live sessions are already
# calling $(BIN), so the swap has to be atomic and the old binary has to stay
# reachable. A hook that fails is silent by design, which is exactly why
# rolling back cannot depend on rebuilding an older checkout first.
reinstall: test ## Reinstall over a running install, keeping the previous binary
	@mkdir -p "$(BINDIR)"
	go build -o "$(BIN).new" ./cmd/pager
	@$(MAKE) --no-print-directory smoke CAND="$(BIN).new"
	@if [ -x "$(BIN)" ]; then cp -p "$(BIN)" "$(BIN).prev"; fi
	@mv "$(BIN).new" "$(BIN)"
	@echo "✓ reinstalled $(BIN)"
	@echo "  previous binary kept at $(BIN).prev — restore it with: make rollback"
	@"$(BIN)" whoami || true

rollback: ## Restore the binary kept by the last reinstall
	@test -x "$(BIN).prev" || { echo "no $(BIN).prev to restore"; exit 1; }
	@mv "$(BIN).prev" "$(BIN)"
	@echo "✓ restored $(BIN) from .prev"
	@$(MAKE) --no-print-directory smoke CAND="$(BIN)"

test: ## Run all tests with the race detector
	go test -race ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format all Go sources
	go fmt ./...

check: fmt vet test ## Format, vet, then test

clean: ## Remove build output
	@rm -f "$(BIN).new"
	go clean ./...
