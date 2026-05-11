# llm-router Makefile

GO              ?= go
GOFLAGS         ?=
PKG             ?= ./...
COVER_PROFILE   ?= coverage.txt
BIN_DIR         ?= bin

.PHONY: all build test test-race cover lint vet fmt clean \
        router replay gen-traces bench-bin bench help

all: build

help:
	@echo "Targets:"
	@echo "  build       Build all binaries into $(BIN_DIR)/"
	@echo "  test        Run tests"
	@echo "  test-race   Run tests with -race"
	@echo "  cover       Run tests and emit coverage report ($(COVER_PROFILE))"
	@echo "  lint        Run golangci-lint"
	@echo "  vet         Run go vet"
	@echo "  fmt         Run gofmt -s -w on all .go files"
	@echo "  bench       Run the in-process benchmark harness"
	@echo "  clean       Remove build artifacts"

build: router replay gen-traces bench-bin

router:
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/router ./cmd/router

replay:
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/replay ./cmd/replay

gen-traces:
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/gen-traces ./cmd/gen-traces

bench-bin:
	$(GO) build $(GOFLAGS) -o $(BIN_DIR)/bench ./cmd/bench

test:
	$(GO) test $(GOFLAGS) $(PKG)

test-race:
	$(GO) test $(GOFLAGS) -race $(PKG)

cover:
	$(GO) test $(GOFLAGS) -race -covermode=atomic -coverprofile=$(COVER_PROFILE) ./internal/...
	@echo
	@$(GO) tool cover -func=$(COVER_PROFILE) | tail -n 1

lint:
	golangci-lint run

vet:
	$(GO) vet $(PKG)

fmt:
	gofmt -s -w .

bench:
	bash scripts/bench/run.sh

clean:
	rm -rf $(BIN_DIR) $(COVER_PROFILE) coverage.html
