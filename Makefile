SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c

GO_MODULES := control-plane inference-gateway forge forge/test/e2e testkit/e2e control-plane/test/e2e charts/test/e2e
GO_MODULE_FILES := $(foreach module,$(GO_MODULES),$(module)/go.mod $(wildcard $(module)/go.sum))
WORKSPACE_FILES := go.work go.work.sum $(GO_MODULE_FILES)
CONTAINER_TOOL ?= docker

.PHONY: workspace-sync workspace-check workspace-list fmt-check vet build test testkit-test testkit-kind-example e2e-catalogue e2e-catalogue-check lint codegen-check charts-check release-check bump release-security-audit source-authority-check source-authority-audit docker-build check install-hooks pre-commit clean

workspace-sync:
	go work sync

workspace-check:
	@before="$$(shasum $(WORKSPACE_FILES))"; \
	go work sync; \
	after="$$(shasum $(WORKSPACE_FILES))"; \
	if [[ "$$before" != "$$after" ]]; then \
		echo "go work sync changed module metadata; commit the synchronized files"; \
		exit 1; \
	fi
	@$(MAKE) --no-print-directory workspace-list

workspace-list:
	@for module in $(GO_MODULES); do \
		echo ":: independent go list $$module"; \
		(cd "$$module" && GOWORK=off go list ./... >/dev/null); \
	done

fmt-check:
	@for module in control-plane inference-gateway forge; do \
		echo ":: fmt-check $$module"; \
		$(MAKE) -C "$$module" fmt-check; \
	done
	@out=$$(gofmt -l testkit/e2e control-plane/test/e2e charts/test/e2e); \
	if [[ -n "$$out" ]]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	@for module in $(GO_MODULES); do \
		echo ":: go vet $$module"; \
		(cd "$$module" && go vet ./...); \
	done

build:
	$(MAKE) -C control-plane ui-deps build
	$(MAKE) -C inference-gateway build
	$(MAKE) -C forge build

# Full local component tests. Docker is required by control-plane and
# inference-gateway integration tests; infrastructure E2E scenarios remain
# explicit Forge targets and are not run here.
test:
	$(MAKE) testkit-test
	$(MAKE) -C control-plane ui-deps ui-test harness-deps harness-test harness-isolation-test tool-runner-deps tool-runner-test test
	$(MAKE) -C inference-gateway test
	$(MAKE) -C forge test

# Shared mechanics, every owner entrypoint example, and compiled catalogue.
testkit-test:
	cd testkit/e2e && go test -race -count=1 ./...
	$(MAKE) -C control-plane test-e2e-unit
	$(MAKE) -C charts test-e2e-unit
	$(MAKE) -C forge test-e2e-unit
	go run ./testkit/e2e/cmd/e2e-catalogue --format json --output /tmp/iterabase-e2e-catalogue.json
	go run ./testkit/e2e/cmd/e2e-catalogue --format markdown --output /tmp/iterabase-e2e-catalogue.md

testkit-kind-example:
	cd testkit/e2e && go test -race -count=1 -tags=e2e_kind -run '^TestRealKindLifecycle' ./kind

e2e-catalogue:
	go run ./testkit/e2e/cmd/e2e-catalogue --format json

e2e-catalogue-check: testkit-test

lint:
	$(MAKE) -C control-plane ui-deps ui-lint harness-deps harness-lint tool-runner-deps tool-runner-lint lint
	$(MAKE) -C inference-gateway lint
	$(MAKE) -C forge lint
	cd forge/test/e2e && golangci-lint run ./...
	@for module in testkit/e2e control-plane/test/e2e charts/test/e2e; do \
		echo ":: golangci-lint $$module"; \
		(cd "$$module" && golangci-lint run ./...); \
	done

codegen-check:
	$(MAKE) -C control-plane proto-check

charts-check:
	$(MAKE) -C charts check

# Selector, release planning, AWS substrate, remote-content and cache contracts.
release-check:
	python3 .github/scripts/test_affected.py
	python3 .github/scripts/test_aws_ci.py
	python3 .github/scripts/test_fixture_image_cache.py
	python3 .github/scripts/test_remote_content.py
	python3 .github/scripts/test_cache_contract.py
	python3 .github/scripts/test_preview_packages.py
	python3 release/test_bump.py
	python3 release/test_release_plan.py
	python3 .github/scripts/remote_content.py validate

# Bump one release target and every linked version field: make bump TARGET=control-plane VERSION=0.0.41
bump:
	@[[ -n "$(TARGET)" && -n "$(VERSION)" ]] || { echo "usage: make bump TARGET=<target> VERSION=<x.y.z>"; exit 2; }
	python3 release/bump.py "$(TARGET)" "$(VERSION)"

release-security-audit:
	.github/scripts/audit_release_security.sh

source-authority-check:
	.github/scripts/test_source_authority.sh

# Explicit, authenticated post-cutover audit. This is intentionally outside the
# hermetic `check` matrix because it verifies live GitHub and optional registry state.
# The cutover procedure overrides SOURCE_AUTHORITY_STATE while repositories are live.
source-authority-audit:
	.github/scripts/audit_source_authority.sh "$${SOURCE_AUTHORITY_STATE:-archived}"

# Preserve the existing runtime image names while using component-scoped
# monorepo build contexts.
docker-build:
	$(CONTAINER_TOOL) build -t control-plane:latest control-plane
	$(CONTAINER_TOOL) build -t control-plane-harness:latest -f control-plane/harness/Dockerfile control-plane/harness
	$(CONTAINER_TOOL) build -t control-plane-harness-isolation:latest -f control-plane/harness/isolation/Dockerfile control-plane/harness
	$(CONTAINER_TOOL) build -t control-plane-tool-runner:latest -f control-plane/tool-runner/Dockerfile control-plane/tool-runner
	$(CONTAINER_TOOL) build -t inference-gateway:latest inference-gateway

check: workspace-check fmt-check vet build test lint codegen-check charts-check release-check source-authority-check docker-build

install-hooks:
	git config core.hooksPath .githooks

pre-commit: workspace-check fmt-check vet build lint release-check source-authority-check

clean:
	$(MAKE) -C control-plane clean
	$(MAKE) -C inference-gateway clean
	$(MAKE) -C forge clean
	$(MAKE) -C charts clean
