.PHONY: build test test-unit test-e2e test-e2e-workspace test-e2e-gpu test-e2e-unit lint fmt fmt-check install-hooks clean

# Load an optional gitignored local environment file.
-include .env
export

BINARY := bin/forge
GO := go
GOBUILDFLAGS := -trimpath

# Every suite execution records an explicit fixture mode. Required CI supplies
# one verified runtime bundle; the explicit published file remains local-only.
ITERABASE_E2E_FIXTURE_MODE ?= source
ITERABASE_E2E_SOURCE_SHA ?= $(shell git rev-parse HEAD)
ITERABASE_E2E_SOURCE_DIRTY ?= $(if $(shell git status --porcelain),true,false)
ITERABASE_E2E_PUBLISHED_FIXTURE ?= $(CURDIR)/test/e2e/published-fixture.json
ITERABASE_E2E_SOURCE_INPUTS ?= $(ITERABASE_E2E_PUBLISHED_FIXTURE)
export ITERABASE_E2E_FIXTURE_MODE ITERABASE_E2E_SOURCE_SHA ITERABASE_E2E_SOURCE_DIRTY ITERABASE_E2E_PUBLISHED_FIXTURE

VERSION := $(shell version="$$(git describe --tags --match 'forge-v*' --always --dirty 2>/dev/null)" && printf '%s\n' "$$version" | sed 's/^forge-//' || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X github.com/nunocgoncalves/iterabase-mono/forge/internal/version.version=$(VERSION) \
           -X github.com/nunocgoncalves/iterabase-mono/forge/internal/version.commit=$(COMMIT) \
           -X github.com/nunocgoncalves/iterabase-mono/forge/internal/version.date=$(DATE)

build:
	$(GO) build $(GOBUILDFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/forge

test: test-unit

test-unit:
	$(GO) test -race -count=1 ./...

# The E2E module has one top-level TestE2E runner. -run selects an isolated
# scenario; -v is required so capacity skips and named stages remain visible.
test-e2e:
	cd test/e2e && ITERABASE_E2E_SOURCE_INPUTS="$(ITERABASE_E2E_SOURCE_INPUTS)" go test -v -race -count=1 -timeout 95m -run '^TestE2E$$/^cpu$$' .

test-e2e-workspace:
	cd test/e2e && ITERABASE_E2E_SOURCE_INPUTS="$(ITERABASE_E2E_SOURCE_INPUTS)" \
		go test -v -race -count=1 -timeout 95m -run '^TestE2E$$/^cpu-workspace$$' .

test-e2e-gpu:
	cd test/e2e && ITERABASE_E2E_SOURCE_INPUTS="$(ITERABASE_E2E_SOURCE_INPUTS)" go test -v -race -count=1 -timeout 100m -run '^TestE2E$$/^gpu$$' .

# Run the hermetic shared-runner scenario and Forge-specific fixture,
# diagnostics, GPU-transition, and candidate-composition tests. Direct chart
# install/product Kind scenarios live only in their authoritative owner suites.
test-e2e-unit:
	cd test/e2e && go test -race -count=1 -run '^TestE2E$$/^hermetic-example$$' .
	cd test/e2e && go test -race -count=1 -skip '^TestE2E$$' ./...

lint:
	golangci-lint run ./...

fmt:
	$(GO) fmt ./...

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

install-hooks:
	git config core.hooksPath .githooks

clean:
	rm -rf bin/
	$(GO) clean -testcache
