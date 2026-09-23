# Image URL to use for building/pushing image targets (matches config/manager).
IMG ?= controller:latest

# Setting SHELL to bash allows bash commands to be executed by recipes.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
CONTAINER_TOOL ?= docker

# Version metadata injected into binaries via linker flags.
VERSION := $(shell version="$$(git describe --tags --match 'control-plane-v*' --always --dirty 2>/dev/null)" && printf '%s\n' "$$version" | sed 's/^control-plane-//' || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X github.com/nunocgoncalves/iterabase-mono/control-plane/internal/version.version=$(VERSION) \
           -X github.com/nunocgoncalves/iterabase-mono/control-plane/internal/version.commit=$(COMMIT) \
           -X github.com/nunocgoncalves/iterabase-mono/control-plane/internal/version.date=$(DATE)

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd:allowDangerousTypes=true webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: fmt-check
fmt-check: ## Fail if gofmt would change anything.
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test-unit
test-unit: ## Run unit tests (skips integration/Docker tests via -short).
	go test -short -race -count=1 ./...

ENVTEST_DEPENDENCY := $(if $(KUBEBUILDER_ASSETS),,setup-envtest)
.PHONY: test
test: manifests generate fmt vet $(ENVTEST_DEPENDENCY) ## Run tests (unit + envtest + integration) fresh, without cached results.
	@assets="$(KUBEBUILDER_ASSETS)"; \
	if [ -z "$$assets" ]; then assets="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)"; fi; \
	KUBEBUILDER_ASSETS="$$assets" go test -count=1 -coverprofile cover.out ./...

E2E_SOURCE_SHA ?= $(shell git rev-parse HEAD)
E2E_SOURCE_DIRTY ?= $(if $(shell git status --porcelain),true,false)
ITERABASE_E2E_FIXTURE_MODE ?= source
E2E_ENV := ITERABASE_E2E_FIXTURE_MODE=$(ITERABASE_E2E_FIXTURE_MODE) \
	ITERABASE_E2E_SOURCE_SHA=$(E2E_SOURCE_SHA) \
	ITERABASE_E2E_SOURCE_DIRTY=$(E2E_SOURCE_DIRTY) \
	ITERABASE_REPOSITORY_ROOT=$(abspath ..)
# Required source and candidate runs receive the same composer-produced bundle.
E2E_BUILD_DEPS :=

PLAYWRIGHT_INSTALL_ARGS ?= chromium

.PHONY: test-e2e-unit test-e2e test-e2e-identity test-e2e-work test-e2e-artifact test-e2e-execution test-e2e-browser playwright-browser-deps

test-e2e-unit: ## Compile the owner E2E entrypoint and run hermetic helper tests.
	cd test/e2e && $(E2E_ENV) go test -race -count=1 -run '^TestE2E$$/^hermetic-example$$' .
	cd test/e2e && $(E2E_ENV) go test -race -count=1 -skip '^TestE2E$$' .
	cd test/e2e && go test -race -count=1 ./fixtures/runtime

# Each product contract gets a fresh isolated Kind cluster and consumes the
# same exact composer-produced runtime bundle in source and candidate modes.
test-e2e: test-e2e-identity test-e2e-work test-e2e-artifact test-e2e-execution test-e2e-browser

test-e2e-identity: $(E2E_BUILD_DEPS)
	cd test/e2e && $(E2E_ENV) go test -v -race -count=1 -timeout 30m -run '^TestE2E$$/^deployed-identity-api$$' .

test-e2e-work: $(E2E_BUILD_DEPS)
	cd test/e2e && $(E2E_ENV) go test -v -race -count=1 -timeout 35m -run '^TestE2E$$/^deployed-work-recovery$$' .

test-e2e-artifact: $(E2E_BUILD_DEPS)
	cd test/e2e && $(E2E_ENV) go test -v -race -count=1 -timeout 35m -run '^TestE2E$$/^deployed-artifact-durability$$' .

test-e2e-execution: $(E2E_BUILD_DEPS)
	cd test/e2e && $(E2E_ENV) go test -v -race -count=1 -timeout 55m -run '^TestE2E$$/^deployed-execution-contracts$$' .

playwright-browser-deps:
	cd test/e2e/playwright && npm ci && npm run lint
	@if [ "$(E2E_SKIP_BUILD_DEPS)" = true ]; then \
		test -n "$$PLAYWRIGHT_BROWSERS_PATH"; \
		test -f "$$PLAYWRIGHT_BROWSERS_PATH/chromium-1234/INSTALLATION_COMPLETE"; \
		test -f "$$PLAYWRIGHT_BROWSERS_PATH/chromium_headless_shell-1234/INSTALLATION_COMPLETE"; \
		test -f "$$PLAYWRIGHT_BROWSERS_PATH/ffmpeg-1011/INSTALLATION_COMPLETE"; \
	else \
		cd test/e2e/playwright && npx --no-install playwright install $(PLAYWRIGHT_INSTALL_ARGS); \
	fi

test-e2e-browser: $(E2E_BUILD_DEPS) playwright-browser-deps
	cd test/e2e && $(E2E_ENV) go test -v -race -count=1 -timeout 45m -run '^TestE2E$$/^deployed-browser-journeys$$' .

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run ./...

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix ./...

##@ Build

.PHONY: build
build: ui-build vet ## Build Dashboard, manager, api, gateway, and dispatch binaries.
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/manager ./cmd/manager
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/api ./cmd/api
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/gateway ./cmd/gateway
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/dispatch ./cmd/dispatch

.PHONY: run-manager
run-manager: ## Run the operator from your host.
	go run ./cmd/manager

.PHONY: run-api
run-api: ## Run the API (serve) from your host. Requires DATABASE_URL.
	go run ./cmd/api serve

.PHONY: run-gateway
run-gateway: ## Run the tool gateway (serve) from your host. Requires DATABASE_URL + mTLS certs.
	go run ./cmd/gateway serve

.PHONY: run-dispatch
run-dispatch: ## Run the dispatch Work server (serve) from your host. Requires DATABASE_URL + mTLS certs.
	go run ./cmd/dispatch serve

.PHONY: migrate-up
migrate-up: ## Apply database migrations.
	go run ./cmd/api migrate up

.PHONY: migrate-down
migrate-down: ## Roll back the last N migrations (default: all).
	go run ./cmd/api migrate down

.PHONY: docker-build
docker-build: ## Build docker image with both binaries.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image.
	$(CONTAINER_TOOL) push ${IMG}

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

##@ Deployment (dev/envtest only — prod is forge Helm)

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster in ~/.kube/config.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dashboard (React/Vite — HOR-396)

.PHONY: ui-deps
ui-deps: ## Install locked Dashboard dependencies.
	cd ui && npm ci

.PHONY: ui-build
ui-build: ## Typecheck and build the embedded Dashboard.
	cd ui && npm run build

.PHONY: ui-lint
ui-lint: ## Typecheck the Dashboard.
	cd ui && npm run lint

.PHONY: ui-test
ui-test: ## Run Dashboard component, interaction, a11y, and localization tests.
	cd ui && npm test

##@ Harness (Node pi harness — HOR-351 / HOR-381 warm-worker refactor)

HARNESS_IMG ?= control-plane-harness:latest
HARNESS_ISOLATION_IMG ?= control-plane-harness-isolation:latest
BUF ?= $(LOCALBIN)/buf
PROTOC_GEN_GO ?= $(LOCALBIN)/protoc-gen-go
PROTOC_GEN_CONNECT_GO ?= $(LOCALBIN)/protoc-gen-connect-go
PROTOC_GEN_ES ?= $(LOCALBIN)/protoc-gen-es
BUF_VERSION ?= v1.72.0
PROTOC_GEN_GO_VERSION ?= v1.36.12
PROTOC_GEN_CONNECT_GO_VERSION ?= v1.20.0
PROTOC_GEN_ES_VERSION ?= 2.13.0

.PHONY: proto-tools buf-tool protoc-gen-go-tool protoc-gen-connect-go-tool protoc-gen-es-tool
proto-tools: buf-tool protoc-gen-go-tool protoc-gen-connect-go-tool protoc-gen-es-tool ## Install pinned buf + protoc plugins (proto lint + codegen).
	@PATH="$(LOCALBIN):$$PATH" $(BUF) --version

buf-tool: $(LOCALBIN)
	$(call go-install-tool,$(BUF),github.com/bufbuild/buf/cmd/buf,$(BUF_VERSION),../.github/tools/control-plane)

protoc-gen-go-tool: $(LOCALBIN)
	$(call go-install-tool,$(PROTOC_GEN_GO),google.golang.org/protobuf/cmd/protoc-gen-go,$(PROTOC_GEN_GO_VERSION),../.github/tools/control-plane)

protoc-gen-connect-go-tool: $(LOCALBIN)
	$(call go-install-tool,$(PROTOC_GEN_CONNECT_GO),connectrpc.com/connect/cmd/protoc-gen-connect-go,$(PROTOC_GEN_CONNECT_GO_VERSION),../.github/tools/control-plane)

protoc-gen-es-tool: $(LOCALBIN)
	@if [ "$(ITERABASE_CI_EXACT_TOOLS)" = true ]; then \
		test -x "$(PROTOC_GEN_ES)"; \
	else \
		npm ci --ignore-scripts --prefix ../.github/tools/protobuf; \
		ln -sf "$$(realpath ../.github/tools/protobuf/node_modules/.bin/protoc-gen-es)" "$(PROTOC_GEN_ES)"; \
	fi

.PHONY: proto
proto: proto-tools ## Generate Go+TS stubs from proto/: harness -> internal/harnessrpc + harness/src/gen; gateway -> internal/gatewayrpc.
	cd proto && PATH="$(LOCALBIN):$$PATH" $(BUF) lint \
		&& PATH="$(LOCALBIN):$$PATH" $(BUF) generate --path iterabase/harness \
		&& PATH="$(LOCALBIN):$$PATH" $(BUF) generate --template buf.gateway.gen.yaml --path iterabase/gateway

.PHONY: proto-check
proto-check: proto ## CI guard: proto lints clean and generated code is fresh (regenerate + diff).
	@git diff --exit-code -- internal/harnessrpc internal/gatewayrpc harness/src/gen tool-runner/src/gen || { \
		echo "generated code is stale; run 'make proto' and commit"; exit 1; }

.PHONY: harness-deps
harness-deps: ## Install locked harness Node dependencies.
	cd harness && npm ci

.PHONY: harness-build
harness-build: ## Build the harness (tsc -> dist).
	cd harness && npm run build

HARNESS_TESTSERVER_BIN ?= bin/harness-testserver

.PHONY: harness-test
harness-test: ## Run harness unit + integration tests (vitest). Includes the native gRPC+mTLS Go testserver integration suite (harness/testserver); the Go binary is pre-built here so the test doesn't time out building/downloading modules. Skipped automatically when go is absent.
	go build -o $(HARNESS_TESTSERVER_BIN) ./harness/testserver
	cd harness && HARNESS_TESTSERVER_BIN=../$(HARNESS_TESTSERVER_BIN) npm test

.PHONY: harness-integration-test
harness-integration-test: ## Build + run only the HOR-381 native gRPC+mTLS transport integration test (Go connect-go testserver + real TS client).
	go build -o $(HARNESS_TESTSERVER_BIN) ./harness/testserver
	cd harness && HARNESS_TESTSERVER_BIN=../$(HARNESS_TESTSERVER_BIN) npx vitest --run src/testserver.test.ts

.PHONY: harness-lint
harness-lint: ## Typecheck the harness (tsc --noEmit).
	cd harness && npm run lint

.PHONY: harness-image
harness-image: ## Build the harness container image.
	$(CONTAINER_TOOL) build -t $(HARNESS_IMG) -f harness/Dockerfile harness/

.PHONY: harness-isolation-image harness-isolation-test harness-isolation-negative-test
harness-isolation-image: ## Build the Linux sandbox isolation fixture image.
	$(CONTAINER_TOOL) build -t $(HARNESS_ISOLATION_IMG) -f harness/isolation/Dockerfile harness/

harness-isolation-test: harness-isolation-image ## Freshly format real ext4+XFS filesystems and require the production setpriv/UID/TLS-key/fresh-process isolation gate; missing privileged Linux prerequisites fail.
	$(CONTAINER_TOOL) run --rm --privileged -e HARNESS_ISOLATION_FILESYSTEM=ext4 $(HARNESS_ISOLATION_IMG)
	$(CONTAINER_TOOL) run --rm --privileged -e HARNESS_ISOLATION_FILESYSTEM=xfs $(HARNESS_ISOLATION_IMG)

harness-isolation-negative-test: harness-isolation-image ## Prove an intentional cross-session access break fails the real ext4 isolation gate.
	@if $(CONTAINER_TOOL) run --rm --privileged -e HARNESS_ISOLATION_FILESYSTEM=ext4 -e HARNESS_ISOLATION_BREAK=cross-session-read $(HARNESS_ISOLATION_IMG); then \
		echo "intentional cross-session access break unexpectedly passed"; \
		exit 1; \
	else \
		echo "intentional cross-session access break failed the isolation gate as expected"; \
	fi

##@ Tool runner (Node 24 trusted gateway runner — HOR-397)

.PHONY: tool-runner-deps
tool-runner-deps: ## Install locked tool-runner dependencies.
	cd tool-runner && npm ci

.PHONY: tool-runner-build
tool-runner-build: ## Build the tool runner + Flux materializer.
	cd tool-runner && npm run build

.PHONY: tool-runner-lint
tool-runner-lint: ## Typecheck the tool runner.
	cd tool-runner && npm run lint

.PHONY: tool-runner-test
tool-runner-test: ## Run manifest/materializer/runner unit tests.
	cd tool-runner && npm test

TOOL_RUNNER_IMG ?= control-plane-tool-runner:latest
.PHONY: tool-runner-image
tool-runner-image: ## Build the generic Node 24 tool-runner image.
	$(CONTAINER_TOOL) build -t $(TOOL_RUNNER_IMG) -f tool-runner/Dockerfile tool-runner/

##@ Tooling

.PHONY: install-hooks
install-hooks: ## Install git hooks (.githooks/pre-commit).
	git config core.hooksPath .githooks

.PHONY: clean
clean: ## Remove built binaries and test caches.
	rm -rf bin/ dist/ cover.out ui/dist/assets ui/dist/index.html
	printf 'HOR-396 UI is built into this directory.\n' > ui/dist/placeholder.txt
	go clean -testcache

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.21.0

# setup-envtest is its own module; keep CI/local generation reproducible.
ENVTEST_VERSION ?= v0.25.1

# Exact envtest runtime archive version governed by remote-content.json.
ENVTEST_K8S_VERSION ?= 1.37.0

GOLANGCI_LINT_VERSION ?= v2.12.2

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION),../.github/tools/control-plane)

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION),../.github/tools/control-plane)

.PHONY: setup-envtest
setup-envtest: envtest ## Download envtest binaries for the configured Kubernetes version.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION),../.github/tools/control-plane)

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION),../.github/tools)

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - exact version recorded in the repository tool module
# $4 - repository tool module directory
define go-install-tool
@if [ "$(ITERABASE_CI_EXACT_TOOLS)" = true ]; then \
	test -x "$(1)"; \
else \
	lock_hash="$$(cat "$(4)/go.mod" "$(4)/go.sum" | sha256sum | awk '{print $$1}')"; \
	[ -f "$(1)-$(3)" ] && [ "$$(cat "$(1)-$(3).lock" 2>/dev/null)" = "$$lock_hash" ] || { \
		set -e; \
		echo "Installing repository-locked $(2) $(3)"; \
		rm -f "$(1)" "$(1)-$(3)" "$(1)-$(3).lock"; \
		(cd "$(4)" && GOWORK=off go mod verify && GOWORK=off GOBIN="$(abspath $(LOCALBIN))" go install -mod=readonly "$(2)"); \
		mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)"; \
		printf '%s\n' "$$lock_hash" > "$(1)-$(3).lock"; \
	}; \
	ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"; \
fi
endef
