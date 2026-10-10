.PHONY: build test test-unit test-e2e test-e2e-workspace test-e2e-gpu test-e2e-unit lint fmt fmt-check install-hooks clean

# Load an optional gitignored local environment file.
-include .env
export

BINARY := bin/forge
GO := go
GOBUILDFLAGS := -trimpath

# Every suite execution records the explicit source fixture. Required CI exports
# the exact source identity and source-built artifacts.
ITERABASE_E2E_FIXTURE_MODE ?= source
ITERABASE_E2E_SOURCE_SHA ?= $(shell git rev-parse HEAD)
ITERABASE_E2E_SOURCE_DIRTY ?= $(if $(shell git status --porcelain),true,false)
export ITERABASE_E2E_FIXTURE_MODE ITERABASE_E2E_SOURCE_SHA ITERABASE_E2E_SOURCE_DIRTY

VERSION := $(shell version="$$(git describe --tags --match 'forge-v*' --always --dirty 2>/dev/null)" && printf '%s\n' "$$version" | sed 's/^forge-//' || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X github.com/nunocgoncalves/iterabase-mono/forge/internal/version.version=$(VERSION) \
           -X github.com/nunocgoncalves/iterabase-mono/forge/internal/version.commit=$(COMMIT) \
           -X github.com/nunocgoncalves/iterabase-mono/forge/internal/version.date=$(DATE)

build:
	$(GO) build $(GOBUILDFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/forge

test: test-unit

# Tests must never touch the operator's ~/.forge (HOR-644). The suite runs with
# a throwaway HOME and FORGE_HOME unset, so a test that falls back to the
# default state root writes into the throwaway home and fails this target
# instead of polluting the real audit log. Go keeps its real caches and config.
test-unit:
	@tmp_home="$$(mktemp -d)" && trap 'rm -rf "$$tmp_home"' EXIT && \
	go_cache="$$($(GO) env GOCACHE)" && go_modcache="$$($(GO) env GOMODCACHE)" && \
	go_path="$$($(GO) env GOPATH)" && go_env="$$($(GO) env GOENV)" && \
	echo "$(GO) test -race -count=1 ./... (HOME=$$tmp_home, FORGE_HOME unset)" && \
	env -u FORGE_HOME HOME="$$tmp_home" GOCACHE="$$go_cache" GOMODCACHE="$$go_modcache" \
		GOPATH="$$go_path" GOENV="$$go_env" $(GO) test -race -count=1 ./... && \
	if [ -e "$$tmp_home/.forge" ]; then \
		echo "a test wrote Forge state under the default \$$HOME/.forge root; isolate it with FORGE_HOME or t.TempDir():" >&2; \
		find "$$tmp_home/.forge" >&2; \
		exit 1; \
	fi

# The E2E module has one top-level TestE2E runner. -run selects an isolated
# scenario; -v is required so capacity skips and named stages remain visible.
test-e2e:
	cd test/e2e && go test -v -race -count=1 -timeout 60m -run '^TestE2E$$/^cpu$$' .

test-e2e-workspace:
	cd test/e2e && go test -v -race -count=1 -timeout 120m -run '^TestE2E$$/^cpu-workspace$$' .

test-e2e-gpu:
	cd test/e2e && go test -v -race -count=1 -timeout 90m -run '^TestE2E$$/^gpu$$' .

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
