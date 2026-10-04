SHELL := /bin/bash
.DEFAULT_GOAL := help

MODULE  := github.com/ahmedmohamedut/kumo
GO      ?= go
NPM     ?= npm
WEB     := web
BIN     := bin/kumod
EMBED   := internal/webui/dist
PKGS    := ./...

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)

# Kumo itself is pure Go and must build without cgo (AGENTS.md, architecture
# rule 7). Only the build targets force CGO_ENABLED=0: `go test -race` needs cgo.
NO_CGO := CGO_ENABLED=0

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: deps
deps: ## Download Go and npm dependencies
	$(GO) mod download
	$(NPM) --prefix $(WEB) ci

.PHONY: dev
dev: ## Run the API on :8080 (air) and Vite on :5173
	@trap 'kill 0' EXIT INT TERM; \
	$(GO) tool air -c .air.toml & \
	$(NPM) --prefix $(WEB) run dev & \
	wait

.PHONY: run
run: ## Run kumod in the foreground
	$(NO_CGO) $(GO) run ./cmd/kumod

.PHONY: build
build: web-build ## Build the console and embed it into bin/kumod
	$(NO_CGO) $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/kumod
	@echo "built $(BIN) ($(VERSION))"

.PHONY: web-build
web-build: ## Build the frontend and stage it for embedding in the binary
	$(NPM) --prefix $(WEB) run build
	rm -rf $(EMBED)
	mkdir -p $(EMBED)
	cp -R $(WEB)/dist/. $(EMBED)/
	touch $(EMBED)/.gitkeep

.PHONY: test
test: ## Run Go and frontend tests
	$(GO) test $(PKGS) -race -count=1
	$(NPM) --prefix $(WEB) run test --if-present

.PHONY: test-libvirt
test-libvirt: ## Run the opt-in integration tests against a real libvirtd
	$(GO) test -tags libvirt $(PKGS) -count=1 -timeout 30m

# TestLaunchBootsCirrOS downloads and boots a guest, so it is opt-in: point
# KUMO_TEST_CIRROS_URL at a CirrOS cloud image (the 0.6.x qcow2 files) to run it.
# KUMO_TEST_LIBVIRT_URI and KUMO_TEST_LIBVIRT_SOCKET override the daemon it uses.

.PHONY: lint
lint: ## Run golangci-lint, oxlint, prettier --check and tsc
	$(GO) tool golangci-lint run
	$(NPM) --prefix $(WEB) run lint
	$(NPM) --prefix $(WEB) run format:check --if-present
	$(NPM) --prefix $(WEB) run typecheck --if-present

.PHONY: fmt
fmt: ## Format Go and frontend sources
	$(GO) tool golangci-lint fmt
	$(NPM) --prefix $(WEB) run format --if-present

.PHONY: migrate-new
migrate-new: ## Create a goose migration: make migrate-new name=add_foo
	./scripts/migrate-new.sh "$(name)"

.PHONY: sqlc
sqlc: ## Regenerate sqlc code from internal/store/queries
	$(GO) tool sqlc generate

.PHONY: openapi
openapi: ## Validate api/openapi.yaml and regenerate the TypeScript types
	$(NPM) --prefix $(WEB) run gen:api --if-present
	# openapi-typescript emits its own formatting, which `make lint` rejects, so
	# the generated file is formatted before the tree is checked again.
	$(NPM) --prefix $(WEB) run format --if-present

.PHONY: version
version: ## Print the version this build would carry
	@echo "$(VERSION) ($(COMMIT))"

.PHONY: clean
clean: ## Remove build output
	$(GO) clean
	rm -f $(BIN)
	rm -rf $(EMBED)
	mkdir -p $(EMBED)
	touch $(EMBED)/.gitkeep