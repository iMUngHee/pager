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

.PHONY: help build test vet fmt check clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-8s\033[0m %s\n",$$1,$$2}'

build: ## Build pager into ~/.local/bin (replace-while-running safe)
	@mkdir -p "$(BINDIR)"
	go build -o "$(BIN).new" ./cmd/pager
	@"$(BIN).new" --help >/dev/null
	@mv "$(BIN).new" "$(BIN)"
	@echo "✓ installed $(BIN)"

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
