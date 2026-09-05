.PHONY: build test test-pg test-pg-down test-pg-project dev clean web dev-web serve restart lint install check vuln web-check web-test web-audit

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

# -timeout matches CI (see .github/workflows/ci.yml). Without it `go test`
# uses a 10m per-test-binary default nobody chose — the shape that killed
# the v0.13.0 release pre-flight (TASK-2545).
test:
	go test -timeout=45m ./... -v

# Run tests against PostgreSQL (starts a container automatically).
#
# THE HOST PORT IS EPHEMERAL (TASK-2708). It was hardcoded to 5445, which let
# exactly one worktree run this target at a time; with concurrent worktrees the
# normal operating mode that produced three incidents in an afternoon, the worst
# of which forged a green-looking gate leg — `go test` exited 2 having run NO
# TESTS because the container was unreachable, and "exit 2 with zero FAIL lines"
# reads a lot like a pass to a quick glance.
#
# So: Docker assigns the port, we read it back, and we REFUSE TO RUN rather than
# let an unreachable database look like a test result. Never put a fixed host
# port back in docker-compose.test.yml or hardcode one here.
#
# AN INTERRUPT TEARS THE STACK DOWN TOO. Ctrl-C during `go test` kills this
# recipe's shell, and without the trap above `down -v` never runs — orphaning
# exactly the stack this task was filed about. Set before `up`, so an interrupt
# during startup is covered as well.
#
# WHAT THIS DELIBERATELY DOES NOT DO: report a database that dies AFTER a
# passing run. The post-run banner is gated on the tests having failed, and a
# reviewer asked for that gate to be dropped. It stays, because the premise
# does not hold — storetest.NewPostgres skips only when PAD_TEST_POSTGRES_URL
# is EMPTY, and a database that is gone produces t.Fatalf, not a skip. So under
# this target a dead database always fails the tests that touch it, and exit 0
# means every Postgres-backed test completed against a live one. Failing the
# leg on a container that stopped after the suite finished would turn honest
# greens red.
#
# THE BANNER IS THE DISCRIMINATOR, NOT THE EXIT CODE. make collapses every
# failed recipe to exit 2, so "the database was unreachable" and "tests failed"
# are indistinguishable by status — measured, not assumed. Do not key automation
# off the exit code expecting to tell them apart; grep the output for
# NO TESTS EXECUTED (nothing ran) or THE DATABASE DIED (it ran against a
# database that went away).
#
# Recovering an orphan (a stack whose worktree was removed before teardown):
# `docker ps --filter name=padtest-` lists them, and the container name carries
# the project. Reap one from anywhere with
#   docker compose -p <project> down -v
# From inside the worktree itself, `make test-pg-project` prints the name and
# `make test-pg-down` does it for you.
# TEST_PG_PKGS narrows the run. Defaults to everything, which is what a gate
# wants; a narrower value is for checking this target's own plumbing (e.g. the
# concurrency acceptance) without two full-suite runs. A gate leg reported from
# a narrowed run is not a gate leg.
TEST_PG_PKGS ?= ./...

# An EXPLICIT project name, unique per absolute path (codex round 1, P2).
# Compose otherwise defaults it to the directory BASENAME, so two checkouts
# that happen to share a basename — /a/docapp and /b/docapp — share a stack,
# and one `down -v` tears down the other's database mid-run. That is the
# cross-worktree teardown this task exists to make impossible, reachable
# through a second door. The basename is kept in the name so an orphan is
# still identifiable by eye; the checksum of the full path is what makes it
# unique. Lowercased and punctuation-stripped because compose rejects
# anything else.
# $$PWD and pwd rather than $(CURDIR): make interpolates CURDIR into the
# command TEXT, so a checkout path containing a quote would break the quoting
# and run whatever followed it. The shell reads its own working directory
# instead, so no path text is ever parsed as command text. cksum is 32-bit and
# that is deliberate — it is portable to every platform this repo builds on,
# unlike sha1sum/shasum, and the basename is in the name too, so the checksum
# only has to separate same-named siblings rather than be cryptographic.
# `tr -d '\n'` BEFORE the -c translation, not after: `tr -c` treats the
# trailing newline basename emits as an invalid character and turns it into a
# dash, which produced `padtest-docapp-2708--1800854141`. Compose accepted it,
# so nothing failed — the doubled dash in the printed name is what showed it.
TEST_PG_PROJECT := padtest-$(shell basename "$$PWD" | tr -d '\n' | tr 'A-Z' 'a-z' | tr -c 'a-z0-9_-' '-')-$(shell pwd | cksum | cut -d' ' -f1)
COMPOSE_TEST := docker compose -p $(TEST_PG_PROJECT) -f docker-compose.test.yml

test-pg:
	@trap '$(COMPOSE_TEST) down -v >/dev/null 2>&1; echo ""; echo "test-pg: INTERRUPTED - stack $(TEST_PG_PROJECT) torn down."; exit 130' INT TERM; \
	port=""; \
	if ! $(COMPOSE_TEST) up -d --wait; then \
		echo ""; \
		echo "################################################################"; \
		echo "# NO TESTS EXECUTED - the database container never came up      #"; \
		echo "# This is NOT a test result. The Postgres leg did not run.      #"; \
		echo "################################################################"; \
		$(COMPOSE_TEST) down -v >/dev/null 2>&1 || echo "# ...and TEARDOWN ALSO FAILED: docker compose -p $(TEST_PG_PROJECT) down -v"; \
		exit 1; \
	fi; \
	port=$$($(COMPOSE_TEST) port postgres 5432 2>/dev/null | sed 's/.*://'); \
	if [ -z "$$port" ]; then \
		echo ""; \
		echo "################################################################"; \
		echo "# NO TESTS EXECUTED - could not read the container's host port  #"; \
		echo "# This is NOT a test result. The Postgres leg did not run.      #"; \
		echo "################################################################"; \
		$(COMPOSE_TEST) down -v >/dev/null 2>&1 || echo "# ...and TEARDOWN ALSO FAILED: docker compose -p $(TEST_PG_PROJECT) down -v"; \
		exit 1; \
	fi; \
	url="postgres://pad:pad@127.0.0.1:$$port/pad?sslmode=disable"; \
	if command -v pg_isready >/dev/null 2>&1; then \
		pg_isready -h 127.0.0.1 -p "$$port" -U pad -q; ready=$$?; \
	else \
		$(COMPOSE_TEST) exec -T postgres pg_isready -U pad -q; ready=$$?; \
	fi; \
	if [ $$ready -ne 0 ]; then \
		echo ""; \
		echo "################################################################"; \
		echo "# NO TESTS EXECUTED - Postgres on port $$port is not ready      #"; \
		echo "# This is NOT a test result. The Postgres leg did not run.      #"; \
		echo "################################################################"; \
		$(COMPOSE_TEST) down -v >/dev/null 2>&1 || echo "# ...and TEARDOWN ALSO FAILED: docker compose -p $(TEST_PG_PROJECT) down -v"; \
		exit 1; \
	fi; \
	echo "test-pg: Postgres on 127.0.0.1:$$port (project $(TEST_PG_PROJECT))"; \
	PAD_TEST_POSTGRES_URL="$$url" go test -timeout=45m $(TEST_PG_PKGS) -v -count=1; \
	EXIT_CODE=$$?; \
	if command -v pg_isready >/dev/null 2>&1; then \
		pg_isready -h 127.0.0.1 -p "$$port" -U pad -q; still_up=$$?; \
	else \
		$(COMPOSE_TEST) exec -T postgres pg_isready -U pad -q; still_up=$$?; \
	fi; \
	if [ $$EXIT_CODE -ne 0 ] && [ $$still_up -ne 0 ]; then \
		echo ""; \
		echo "################################################################"; \
		echo "# THE DATABASE DIED DURING THE RUN.                             #"; \
		echo "# Treat the failures above as INFRASTRUCTURE, not as evidence   #"; \
		echo "# about the code, and re-run before drawing any conclusion.     #"; \
		echo "################################################################"; \
	fi; \
	if ! $(COMPOSE_TEST) down -v; then \
		echo ""; \
		echo "################################################################"; \
		echo "# TEARDOWN FAILED - the stack is still running. Reap it with:    #"; \
		echo "#   docker compose -p $(TEST_PG_PROJECT) down -v"; \
		echo "# The test status below is honest; this leak is a separate fact. #"; \
		echo "################################################################"; \
	fi; \
	exit $$EXIT_CODE

# Tears down THIS directory's stack only: the project name is keyed to this
# directory's ABSOLUTE path, so it cannot reach a sibling's container even if
# the two directories share a basename.
test-pg-down:
	$(COMPOSE_TEST) down -v

# Prints this directory's compose project name, so an orphan can be reaped
# from anywhere without re-deriving it by hand.
test-pg-project:
	@echo $(TEST_PG_PROJECT)

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

# Web pre-flight that mirrors CI's Web job beyond the build step: svelte-check
# type checking. Depends on `web` so npm ci + build are already done.
# Separate target so a contributor iterating on the UI can run just the
# extra checks via `make web-check`.
web-check: web
	cd web && npm run check

# Run the web unit-test suite (vitest, non-watch). Mirrors the "Run web unit
# tests" step in CI's Web job. Kept separate from web-check so a contributor
# can run just the vitest suite via `make web-test`; `check` invokes both.
web-test:
	cd web && npm run test

# `npm audit` (production dependencies, high severity+), through the same
# gate CI uses — web/scripts/ci-audit.mjs — and, like CI, LAST (BUG-2881).
# Bare `npm audit` exits non-zero the same way for "an advisory exists" and
# "the advisory service was unreachable", and with `&&` in front of the other
# checks a registry blip used to stop svelte-check and vitest from running at
# all. The script tells the two apart, retries the second, and fails closed
# under its own title; running it after the correctness checks means their
# verdict exists whichever way it goes.
#
# NO `web` prerequisite (codex round 4 on #1247): `npm audit` reads the
# lockfile and needs neither node_modules nor a build, and `web` is .PHONY,
# so depending on it made `check` run `npm ci` twice and made this the one
# new target to reach `npm ci` — the command CLAUDE.md forbids in a worktree
# with a symlinked node_modules. Standalone, it is safe to run anywhere.
web-audit:
	cd web && npm run audit:ci

# Pre-flight target that mirrors CI's Go and Web jobs. Run this before
# pushing — if it passes, the corresponding CI checks should pass too.
#
# Covers: golangci-lint suite (lint), Go test suite, govulncheck, npm ci,
# web build, svelte-check, vitest unit tests, npm audit (last, see web-audit). The race-detector +
# Postgres jobs only run on push to main (per .github/workflows/ci.yml) and
# are not included here; run `make test-pg` separately if you want them
# locally.
#
# `make install` stays lightweight (build + restart only) so the inner
# dev loop is fast; opt into `check` when you're ready to push.
check: lint
	go test -timeout=45m ./...
	$(MAKE) vuln
	$(MAKE) web-check
	$(MAKE) web-test
	$(MAKE) web-audit
