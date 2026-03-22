.PHONY: build test dev clean web dev-web serve restart lint install

BINARY=pad
BUILD_DIR=./cmd/pad
HOST?=0.0.0.0
INSTALL_DIR?=$(HOME)/.local/bin

build: web
	@# Write a build timestamp into embed.go so Go's content-hash cache
	@# sees a real change and re-embeds the web assets.
	@printf 'package pad\n\nimport "embed"\n\n//go:embed web/build/*\nvar WebUI embed.FS\n\n//go:embed skills/pad/SKILL.md\nvar PadSkill []byte\n\n// embed cache bust: %s\n' "$$(date +%s)" > embed.go
	go build -o $(BINARY) $(BUILD_DIR)

build-go:
	@printf 'package pad\n\nimport "embed"\n\n//go:embed web/build/*\nvar WebUI embed.FS\n\n//go:embed skills/pad/SKILL.md\nvar PadSkill []byte\n\n// embed cache bust: %s\n' "$$(date +%s)" > embed.go
	go build -o $(BINARY) $(BUILD_DIR)

install: build
	@# Stop running server, install binary, clear stale pid
	-killall -9 $(BINARY) 2>/dev/null
	@sleep 1
	@mkdir -p $(INSTALL_DIR)
	cp -f $(BINARY) $(INSTALL_DIR)/$(BINARY)
	rm -f ~/.pad/pad.pid
	@echo "Installed $(BINARY) to $(INSTALL_DIR)/$(BINARY)"
	@# Trigger server auto-start by running a command
	@$(INSTALL_DIR)/$(BINARY) status 2>/dev/null || true
	@echo "Server restarted."

test:
	go test ./... -v

dev: build-go
	./$(BINARY) serve --host $(HOST)

serve: build
	-./$(BINARY) stop 2>/dev/null
	@sleep 1
	./$(BINARY) serve --host $(HOST)

restart: build-go
	-./$(BINARY) stop 2>/dev/null
	@sleep 1
	./$(BINARY) serve --host $(HOST)

web:
	cd web && npm run build

dev-web:
	cd web && npm run dev

clean:
	rm -f $(BINARY)
	rm -rf web/build
	go clean ./...

lint:
	go vet ./...
