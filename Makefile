.PHONY: build test test-pg test-pg-down dev clean web dev-web serve restart lint install check vuln web-check web-test

BINARY=pad
BUILD_DIR=./cmd/pad
HOST?=127.0.0.1
INSTALL_DIR?=$(HOME)/.local/bin

VERSION   ?= dev
COMMIT    := $(shell git rev-parse --short HEAD 2>/dev/null)
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS   := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildTime=$(BUILD_TIME)

# Pin to the same golangci-lint and govulncheck versions CI runs (see
# .github/workflows/ci.yml). Bump these and CI together when upgrading.
GOLANGCI_LINT_VERSION ?= v2.11.4
GOLANGCI_LINT := $(shell go env GOPATH)/bin/golangci-lint
GOVULNCHECK_VERSION  ?= v1.2.0
GOVULNCHECK := $(shell go env GOPATH)/bin/govulncheck

build: web
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(BUILD_DIR)

build-go:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(BUILD_DIR)

install: build
	@# Stop running server, install binary, clear stale pid.
	@# CAUTION: `pkill -x pad` is SYSTEM-WIDE (matches the binary name on the
	@# whole host). If another user or project on the same machine is running
	@# a `pad` process, it will get signaled too. Designed for single-developer
	@# local setups; don't run `make install` on a shared host.
	@#
	@# SIGTERM (not SIGKILL) so the server's graceful-shutdown path runs:
	@# it closes the event bus, which terminates SSE handler goroutines so
	@# the http.Server can write the final 0-chunk before closing each
	@# stream. SIGKILL drops every open SSE connection mid-write, leaving
	@# every browser tab with `ERR_INCOMPLETE_CHUNKED_ENCODING` and a noisy
	@# reconnect storm. Falls back to SIGKILL after 5s for stuck processes.
	@# BUG-1531 / SSE follow-up.
	-pkill -TERM -x $(BINARY) 2>/dev/null; \
		for i in 1 2 3 4 5; do \
			pgrep -x $(BINARY) >/dev/null 2>&1 || break; \
			sleep 1; \
		done; \
		pkill -KILL -x $(BINARY) 2>/dev/null || true
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

# Run the same golangci-lint suite CI runs (.golangci.yml: govet,
# ineffassign, staticcheck SA*, unused, plus the gofmt formatter with
# simplify: true). The lint suite already includes go vet via the govet
# linter, so we don't double-run it here.
#
# Version enforcement: the recipe checks the installed binary against
# GOLANGCI_LINT_VERSION and reinstalls on mismatch. A file-target
# dependency wouldn't enforce the pin — make only runs the install rule
# when the binary is missing, so an outdated local binary would be
# silently reused and disagree with CI (Codex review on PR #322).
lint:
	@bin="$(GOLANGCI_LINT)"; pin="$(GOLANGCI_LINT_VERSION)"; want="$${pin#v}"; \
	have=$$( $$bin version 2>/dev/null | sed -n 's/.*version \([0-9.]*\) built.*/\1/p' ); \
	if [ "$$have" != "$$want" ]; then \
		echo "Installing golangci-lint $$pin (had: $${have:-none})..."; \
		go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$$pin; \
	fi
	$(GOLANGCI_LINT) run --timeout=5m ./...

# Run govulncheck in BINARY mode against a freshly-built pad binary, NOT
# source mode (`govulncheck ./...`). Source mode builds an SSA call-graph
# over the entire dependency tree (BigQuery / OTel / gRPC / Google Cloud),
# which balloons to multiple GB of RAM and can lock up a memory-constrained
# host (BUG-2084). Binary mode reads the compiled binary's symbol table
# instead: a fraction of the memory, still call-graph-precise (it walks the
# binary's symbol graph), and it detects stdlib vulns from the Go version
# stamped in the binary. Because `pad` is a single binary containing the
# whole codebase (server + CLI), scanning it covers everything; not-called
# module vulns are naturally suppressed since their symbols aren't linked in.
# Mirrors the "Run govulncheck" step in CI's Go job — keep the two in sync.
#
# The build needs web/build to exist for the //go:embed directive. Locally
# `make web` / `make install` provides the real assets; the guard below
# drops a placeholder when it's absent (e.g. a fresh clone) so a standalone
# `make vuln` never fails on the embed. `go install foo@vX.Y.Z` is idempotent
# and rebuilds quickly when the pinned version is already cached.
#
# The scan binary is written to the repo root (real disk), NOT /tmp: some
# hosts mount /tmp as a small tmpfs (RAM-backed), where a large embedded
# binary can hit "no space left" and, worse, consume the very RAM we're
# trying not to exhaust (BUG-2084). It's removed on completion and
# .gitignore'd so an interrupted run can't leave a tracked artifact.
vuln:
	go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	@[ -n "$$(ls -A web/build 2>/dev/null)" ] || { mkdir -p web/build && echo placeholder > web/build/.gitkeep; }
	go build -o pad-vulnscan ./cmd/pad
	$(GOVULNCHECK) -mode binary pad-vulnscan; status=$$?; rm -f pad-vulnscan; exit $$status

# Web pre-flight that mirrors CI's Web job beyond the build step:
# `npm audit` (production dependencies, high severity+) and svelte-check
# type checking. Depends on `web` so npm ci + build are already done.
# Separate target so a contributor iterating on the UI can run just the
# extra checks via `make web-check`.
web-check: web
	cd web && npm audit --audit-level=high --omit=dev && npm run check

# Run the web unit-test suite (vitest, non-watch). Mirrors the "Run web unit
# tests" step in CI's Web job. Kept separate from web-check so a contributor
# can run just the vitest suite via `make web-test`; `check` invokes both.
web-test:
	cd web && npm run test

# Pre-flight target that mirrors CI's Go and Web jobs. Run this before
# pushing — if it passes, the corresponding CI checks should pass too.
#
# Covers: golangci-lint suite (lint), Go test suite, govulncheck, npm ci,
# npm audit, web build, svelte-check, vitest unit tests. The race-detector +
# Postgres jobs only run on push to main (per .github/workflows/ci.yml) and
# are not included here; run `make test-pg` separately if you want them
# locally.
#
# `make install` stays lightweight (build + restart only) so the inner
# dev loop is fast; opt into `check` when you're ready to push.
check: lint
	go test ./...
	$(MAKE) vuln
	$(MAKE) web-check
	$(MAKE) web-test
