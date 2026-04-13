.PHONY: build test test-pg test-pg-down dev clean web dev-web serve restart lint install

BINARY=pad
BUILD_DIR=./cmd/pad
HOST?=127.0.0.1
INSTALL_DIR?=$(HOME)/.local/bin

VERSION   ?= dev
COMMIT    := $(shell git rev-parse --short HEAD 2>/dev/null)
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS   := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildTime=$(BUILD_TIME)

build: web
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(BUILD_DIR)

build-go:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(BUILD_DIR)

install: build
	@# Stop running server, install binary, clear stale pid
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

lint:
	go vet ./...
