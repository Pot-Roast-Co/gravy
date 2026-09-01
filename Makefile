# Gravy — build and check targets.
#
# `make check` is the gate: build, vet, layering rules, lint, test. It must pass before any
# ticket is done (see CLAUDE.md).

BINARY      := gravy
CMD         := ./cmd/gravy
DIST        := dist
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -ldflags "-X main.version=$(VERSION)"
GO          ?= go

.PHONY: all build run test test-race vet fmt fmt-check lint lint-layers check clean tidy

all: build

## build: compile the single binary into dist/
build:
	@mkdir -p $(DIST)
	$(GO) build $(LDFLAGS) -o $(DIST)/$(BINARY) $(CMD)
	@echo "built $(DIST)/$(BINARY) ($(VERSION))"

## run: build and run the TUI
run:
	$(GO) run $(LDFLAGS) $(CMD)

## test: unit tests
test:
	$(GO) test ./...

## test-race: unit tests under the race detector
test-race:
	$(GO) test -race ./...

## vet: go vet
vet:
	$(GO) vet ./...

## fmt: rewrite sources with gofmt
fmt:
	gofmt -w .

## fmt-check: fail if anything is not gofmt-clean
fmt-check:
	@out="$$(gofmt -l . 2>/dev/null)"; \
	if [ -n "$$out" ]; then \
		echo "not gofmt-clean:"; echo "$$out"; echo "run: make fmt"; exit 1; \
	fi
	@echo "gofmt: clean"

## lint-layers: enforce the two layering rules from ARCHITECTURE.md 1.1
lint-layers:
	@$(GO) run ./tools/lintlayers .

## lint: golangci-lint, if installed. Skipped with a warning when it is not, so that
## `make check` passes on a clean checkout without extra tooling. CI always installs it.
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "warning: golangci-lint not installed - skipping (CI runs it)"; \
		echo "  install: https://golangci-lint.run/welcome/install/"; \
	fi

## check: the full gate
check: fmt-check vet lint-layers lint test
	@echo "check: all green"

## tidy: tidy and verify go.mod
tidy:
	$(GO) mod tidy

## clean: remove build output
clean:
	rm -rf $(DIST)
