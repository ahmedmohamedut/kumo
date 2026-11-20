SHELL := /bin/bash
.DEFAULT_GOAL := help

MODULE  := github.com/ahmedmohamedut/kumo
GO      ?= go
NPM     ?= npm
WEB     := web
BIN     := bin/kumod
EMBED   := internal/webui/dist
PKGS    := ./...

# --match 'v*' because the repository also carries engine image tags
# (postgres-18-<date>, .github/workflows/engine-images.yml), and a binary
# stamped with one of those would claim to be an image.
VERSION ?= $(shell git describe --tags --match 'v*' --always --dirty 2>/dev/null || echo dev)
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

# test stages the console first because internal/webui asserts that one is
# embedded. Without the dependency the target passes on a tree that happens to
# hold a build from earlier and fails on a fresh clone, which is the order the
# README sends a new contributor through (`make deps`, then `make test`).
.PHONY: test
test: web-build ## Run Go and frontend tests
	$(GO) test $(PKGS) -race -count=1
	$(NPM) --prefix $(WEB) run test --if-present

# e2e drives a browser against the real binary: the console make build embeds,
# the real API and the task worker, with the in-memory hypervisor standing in
# for KVM. It depends on build because the suite starts bin/kumod itself, and
# it is a separate target from test because it needs a browser installed
# (npm --prefix web exec playwright install chromium).
.PHONY: e2e
e2e: build ## Run the Playwright end-to-end suite against bin/kumod
	$(NPM) --prefix $(WEB) run e2e

.PHONY: test-libvirt
test-libvirt: ## Run the opt-in integration tests against a real libvirtd
	$(GO) test -tags libvirt $(PKGS) -count=1 -timeout 30m

.PHONY: test-rustfs
test-rustfs: ## Run the opt-in object storage tests against a real RustFS (or any S3 server)
	$(GO) test -tags rustfs ./internal/objectstore/... -count=1 -timeout 5m

# test-rustfs needs KUMO_TEST_S3_ENDPOINT, KUMO_TEST_S3_ACCESS_KEY and
# KUMO_TEST_S3_SECRET_KEY. It creates kumo-test-<random> buckets and removes them.

# TestLaunchBootsCirrOS downloads and boots a guest, so it is opt-in: point
# KUMO_TEST_CIRROS_URL at a CirrOS cloud image (the 0.6.x qcow2 files) to run it.
# KUMO_TEST_LIBVIRT_URI and KUMO_TEST_LIBVIRT_SOCKET override the daemon it uses.
#
# TestDatabaseJourney is the managed Postgres end-to-end run (07-databases.md
# M7.7): point KUMO_TEST_ENGINE_IMAGE at the qcow2 that
# packaging/images/postgres-18/build.sh produces. It boots the real engine, so
# it needs a few gigabytes of pool space: KUMO_TEST_DATA_DIR puts the pools
# somewhere other than $TMPDIR, which matters on a host whose /tmp is tmpfs,
# and KUMO_TEST_VPC_CIDR moves its VPC off 10.177.0.0/24. The test creates and
# removes its own network, volumes and domain; the two pools it defines are
# adopted on the next run.
#
#   KUMO_TEST_ENGINE_IMAGE=$PWD/packaging/images/postgres-18/out/*.qcow2 \
#   KUMO_TEST_DATA_DIR=/var/tmp/kumo-it make test-libvirt

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

# GoReleaser is pinned here rather than added to go.mod's tool directives: it
# pulls in a dependency tree the size of Kubernetes and would drag the module's
# own Go requirement up with it. `go run pkg@version` builds it outside this
# module, so go.mod and go.sum stay untouched.
GORELEASER ?= github.com/goreleaser/goreleaser/v2@v2.18.3

.PHONY: release-snapshot
release-snapshot: ## Build the release artifacts locally, without publishing
	$(GO) run $(GORELEASER) release --snapshot --clean --skip=publish
	@echo "artifacts in dist/"

.PHONY: release-check
release-check: ## Validate .goreleaser.yaml
	$(GO) run $(GORELEASER) check

.PHONY: security-config
security-config: ## Configure AppArmor/SELinux for Kumo's pools (run as root)
	./scripts/security-config.sh

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