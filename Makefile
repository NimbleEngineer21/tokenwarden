SHELL := /bin/sh
GO := mise exec -- go

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show targets.
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: setup
setup: ## Install pinned tools and git hooks.
	mise install
	mise exec -- lefthook install

.PHONY: fmt
fmt: ## Format code.
	mise exec -- golangci-lint fmt ./...

.PHONY: lint
lint: ## Run linters.
	mise exec -- golangci-lint run ./...
	if [ -d .github/workflows ]; then mise exec -- actionlint; fi

.PHONY: test
test: ## Run tests with the race detector.
	$(GO) test -race -count=1 ./...

.PHONY: build
build: ## Build all packages.
	$(GO) build ./...

.PHONY: tidy
tidy: ## Fail if go.mod or go.sum is not tidy.
	scripts/check-tidy.sh

.PHONY: vuln
vuln: ## Scan dependencies for known vulnerabilities.
	mise exec -- govulncheck ./...

.PHONY: secrets
secrets: ## Scan the full git history for secrets.
	mise exec -- gitleaks git --redact --no-banner .

.PHONY: check
check: lint test build tidy vuln secrets ## Run every check that CI runs.
