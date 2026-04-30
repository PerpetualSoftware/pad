.PHONY: build test test-pg test-pg-down dev clean web dev-web serve restart lint install check

BINARY=pad
BUILD_DIR=./cmd/pad
HOST?=127.0.0.1
INSTALL_DIR?=$(HOME)/.local/bin

VERSION   ?= dev
COMMIT    := $(shell git rev-parse --short HEAD 2>/dev/null)
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS   := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildTime=$(BUILD_TIME)

# Pin to the same golangci-lint version CI runs (see
# .github/workflows/ci.yml). Bump both places together when upgrading.
GOLANGCI_LINT_VERSION ?= v2.11.4
GOLANGCI_LINT := $(shell go env GOPATH)/bin/golangci-lint

build: web
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(BUILD_DIR)

build-go:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(BUILD_DIR)

install: build
	@# Stop running server, install binary, clear stale pid.
	@# CAUTION: `killall -9 pad` is SYSTEM-WIDE. If another user (or another
	@# project on the same machine) is also running a `pad` process, it will
	@# get killed too. Designed for single-developer local setups; don't run
	@# `make install` on a shared host.
	-killall -9 $(BINARY) 2>/dev/null
	@sleep 1
	@mkdir -p $(INSTALL_DIR)
	cp -f $(BINARY) $(INSTALL_DIR)/$(BINARY)
	rm -f ~/.pad/pad.pid
	@echo "Installed $(BINARY) to $(INSTALL_DIR)/$(BINARY)"
	@# Trigger server auto-start by running a command
	@$(INSTALL_DIR)/$(BINARY) auth whoami 2>/dev/null || true
	@echo "Server restarted."

test:
	go test ./... -v

# Run tests against PostgreSQL (starts a container automatically).
# Uses port 5445 to avoid conflicts with any local PostgreSQL.
test-pg:
	docker compose -f docker-compose.test.yml up -d --wait
	PAD_TEST_POSTGRES_URL="postgres://pad:pad@localhost:5445/pad?sslmode=disable" go test ./... -v -count=1; \
		EXIT_CODE=$$?; \
		docker compose -f docker-compose.test.yml down -v; \
		exit $$EXIT_CODE

test-pg-down:
	docker compose -f docker-compose.test.yml down -v

dev: build-go
	./$(BINARY) server start --host $(HOST)

serve: build
	-./$(BINARY) server stop 2>/dev/null
	@sleep 1
	./$(BINARY) server start --host $(HOST)

restart: build-go
	-./$(BINARY) server stop 2>/dev/null
	@sleep 1
	./$(BINARY) server start --host $(HOST)

web:
	cd web && npm ci && npm run build

dev-web:
	cd web && npm run dev

clean:
	rm -f $(BINARY)
	rm -rf web/build
	go clean ./...

# Run the same golangci-lint suite CI runs (see .golangci.yml). Auto-
# installs the pinned binary on first run so contributors don't have to
# remember a separate setup step. The lint suite already includes
# go vet via the govet linter, so we don't double-run it here.
lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run --timeout=5m ./...

# Bootstrap rule: install golangci-lint into $(go env GOPATH)/bin if it's
# missing or doesn't match the pinned version. `go install` is idempotent
# and respects GOBIN; piping through `command -v` keeps the no-op fast.
$(GOLANGCI_LINT):
	@echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION)..."
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# Pre-flight target that mirrors the Go and Web jobs in CI. Run this
# before pushing — if it passes, the corresponding CI checks should pass
# too. Keeps `make install` lightweight (build + restart only) so the
# inner dev loop stays fast; opt into `check` when you're done.
check: lint
	go test ./...
	cd web && npm run build
