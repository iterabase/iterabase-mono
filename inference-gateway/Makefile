.PHONY: build test lint fmt fmt-check install-hooks run clean

BINARY := bin/gateway
GO := go
GOFLAGS := -trimpath
VERSION := $(shell cat VERSION 2>/dev/null || echo dev)
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/version.version=$(VERSION) \
	-X github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/version.commit=$(COMMIT) \
	-X github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/version.date=$(DATE)

build:
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/gateway
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/central ./cmd/central

test:
	$(GO) test -v -race -count=1 ./...

lint:
	golangci-lint run ./...

fmt:
	$(GO) fmt ./...
	@if command -v goimports >/dev/null 2>&1; then goimports -w .; fi

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	@if command -v goimports >/dev/null 2>&1; then \
		out=$$(goimports -l .); if [ -n "$$out" ]; then echo "goimports needed:"; echo "$$out"; exit 1; fi; \
	fi

install-hooks:
	git config core.hooksPath .githooks

run: build
	$(BINARY) serve

clean:
	rm -rf bin/
	$(GO) clean -testcache
