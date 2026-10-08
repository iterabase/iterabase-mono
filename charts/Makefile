CHARTS_DIR := charts
CHARTS := $(notdir $(wildcard $(CHARTS_DIR)/*))
UMBRELLA := iterabase-platform
CERT_MANAGER_SUBSTRATE := cert-manager-substrate
LVM_STORAGE_SUBSTRATE := lvm-storage-substrate
CONTROLPLANE := control-plane
OBSERVABILITY := observability
RENDER := /tmp/$(UMBRELLA).rendered.yaml
RENDER_CP := /tmp/$(CONTROLPLANE).rendered.yaml
RENDER_TLS := /tmp/$(UMBRELLA).tls.rendered.yaml
RENDER_OBS := /tmp/$(UMBRELLA).observability.rendered.yaml
RENDER_OBS_TLS := /tmp/$(UMBRELLA).observability-tls.rendered.yaml
RENDER_INTERNAL_OBS := /tmp/$(UMBRELLA).internal-observability.rendered.yaml
RENDER_PRIVATE_CP := /tmp/$(UMBRELLA).private-control-plane.rendered.yaml
# Pin Kubernetes schemas to one immutable upstream commit. kubeconform's
# floating `master` default can fail or change independently of this source SHA.
KUBECONFORM_SCHEMA_COMMIT := 14355cdd490a43d21e05985668815a36a6f97da6
KUBECONFORM_SCHEMA_LOCATION := https://raw.githubusercontent.com/yannh/kubernetes-json-schema/$(KUBECONFORM_SCHEMA_COMMIT)/{{.NormalizedKubernetesVersion}}-standalone-strict/{{.ResourceKind}}.json
KUBECONFORM := kubeconform -strict -kubernetes-version 1.31.0 -ignore-missing-schemas -schema-location '$(KUBECONFORM_SCHEMA_LOCATION)'
# Use OPO1's short release name so static TLS checks exercise the upstream
# kube-prometheus-stack fullname path that differs from the historical
# `iterabase` E2E release name.
TLS_TEST_RELEASE := opo1
TLS_TEST_NAMESPACE := portable-system
E2E_SOURCE_SHA ?= $(shell git rev-parse HEAD)
E2E_SOURCE_DIRTY ?= $(if $(shell git status --porcelain),true,false)
ITERABASE_E2E_FIXTURE_MODE ?= source
ITERABASE_E2E_PUBLISHED_FIXTURE ?= $(CURDIR)/test/e2e/published-fixture.json
ITERABASE_E2E_TRANSITION_BASELINES ?= $(CURDIR)/test/e2e/transition-baselines.json
E2E_ENV := ITERABASE_E2E_FIXTURE_MODE=$(ITERABASE_E2E_FIXTURE_MODE) \
	ITERABASE_E2E_SOURCE_SHA=$(E2E_SOURCE_SHA) \
	ITERABASE_E2E_SOURCE_DIRTY=$(E2E_SOURCE_DIRTY) \
	ITERABASE_E2E_PUBLISHED_FIXTURE=$(ITERABASE_E2E_PUBLISHED_FIXTURE) \
	ITERABASE_E2E_TRANSITION_BASELINES=$(ITERABASE_E2E_TRANSITION_BASELINES) \
	ITERABASE_CHARTS_ROOT=$(CURDIR)
ifeq ($(E2E_SKIP_BUILD_DEPS),true)
E2E_BUILD_DEPS :=
else
E2E_BUILD_DEPS := build-deps
endif

.PHONY: test-build-dependency-retry build-deps lint template kubeconform template-controlplane kubeconform-controlplane template-tls kubeconform-tls template-observability kubeconform-observability template-observability-tls kubeconform-observability-tls template-internal-observability kubeconform-internal-observability check-internal-observability template-private-control-plane kubeconform-private-control-plane check-private-control-plane check-certificate-substrate check-lvm-storage-substrate check-service-selectors check-remote-image-digests check-redis-exporter-auth check-artifact-config check-tool-runner check-manager-contract check-workload-ca check-auth-config check-dup-yaml-keys check-metrics-contract check-version-links check-alerts test-alert-rules check-dashboards check-observability-tls check check-tls check-observability test-e2e test-e2e-unit test-e2e-certificate-migration test-e2e-install test-e2e-upgrade test-e2e-feature-enable test-e2e-observability-ingress-recovery test-e2e-reapply-rollback test-e2e-metallb-transition test-e2e-observability test-e2e-observability-tls test-e2e-internal-tls clean

# Compile the owner entrypoint and run hermetic mechanics plus intentional break
# fixtures without provisioning infrastructure.
test-e2e-unit:
	cd test/e2e && $(E2E_ENV) go test -race -count=1 -run '^TestE2E$$/^hermetic-example$$' .
	cd test/e2e && $(E2E_ENV) go test -race -count=1 -run '^TestUnit' .

# Each target creates one fresh isolated Kind cluster and runs exactly once.
# Current 0.4 release paths are clean-install/reapply only. Pre-0.4 transition
# targets remain explicit historical evidence and are not part of this aggregate.
test-e2e: test-e2e-install test-e2e-observability test-e2e-observability-tls test-e2e-internal-tls

test-e2e-certificate-migration: $(E2E_BUILD_DEPS)
	cd test/e2e && $(E2E_ENV) go test -v -race -count=1 -timeout 35m -run '^TestE2E$$/^certificate-ownership-migration$$' .

test-e2e-install: $(E2E_BUILD_DEPS)
	cd test/e2e && $(E2E_ENV) go test -v -race -count=1 -timeout 45m -run '^TestE2E$$/^fresh-install$$' .

test-e2e-upgrade: $(E2E_BUILD_DEPS)
	cd test/e2e && $(E2E_ENV) go test -v -race -count=1 -timeout 40m -run '^TestE2E$$/^n-minus-one-upgrade$$' .

test-e2e-feature-enable: $(E2E_BUILD_DEPS)
	cd test/e2e && $(E2E_ENV) go test -v -race -count=1 -timeout 50m -run '^TestE2E$$/^feature-enable-upgrade$$' .

test-e2e-observability-ingress-recovery: $(E2E_BUILD_DEPS)
	cd test/e2e && $(E2E_ENV) go test -v -race -count=1 -timeout 60m -run '^TestE2E$$/^single-node-observability-ingress-recovery$$' .

test-e2e-reapply-rollback: $(E2E_BUILD_DEPS)
	cd test/e2e && $(E2E_ENV) go test -v -race -count=1 -timeout 45m -run '^TestE2E$$/^reapply-rollback-recovery$$' .

test-e2e-metallb-transition: $(E2E_BUILD_DEPS)
	cd test/e2e && $(E2E_ENV) go test -v -race -count=1 -timeout 55m -run '^TestE2E$$/^metallb-upgrade-reapply$$' .

test-e2e-observability: $(E2E_BUILD_DEPS)
	cd test/e2e && ITERABASE_E2E_RELEASE=opo1 $(E2E_ENV) go test -v -race -count=1 -timeout 40m -run '^TestE2E$$/^observability$$' .

test-e2e-observability-tls: $(E2E_BUILD_DEPS)
	cd test/e2e && ITERABASE_E2E_RELEASE=opo1 $(E2E_ENV) go test -v -race -count=1 -timeout 45m -run '^TestE2E$$/^observability-tls$$' .

test-e2e-internal-tls: $(E2E_BUILD_DEPS)
	cd test/e2e && $(E2E_ENV) go test -v -race -count=1 -timeout 30m -run '^TestE2E$$/^internal-tls$$' .

# control-plane has its own file:// dep (postgresql) and observability has its
# own upstream deps (kube-prometheus-stack + loki); build them first so the
# umbrella vendors each with its nested dependencies baked in.
test-build-dependency-retry:
	bash scripts/test-build-chart-dependency.sh

build-deps:
	bash scripts/build-chart-dependency.sh $(CHARTS_DIR)/$(CONTROLPLANE)
	bash scripts/build-chart-dependency.sh $(CHARTS_DIR)/$(OBSERVABILITY)
	bash scripts/build-chart-dependency.sh $(CHARTS_DIR)/$(CERT_MANAGER_SUBSTRATE)
	bash scripts/build-chart-dependency.sh $(CHARTS_DIR)/$(LVM_STORAGE_SUBSTRATE)
	bash scripts/build-chart-dependency.sh $(CHARTS_DIR)/$(UMBRELLA)

lint: build-deps
	@for c in $(CHARTS); do echo ":: helm lint $$c"; helm lint $(CHARTS_DIR)/$$c || exit 1; done

template: build-deps
	helm template $(UMBRELLA) $(CHARTS_DIR)/$(UMBRELLA) > $(RENDER)
	@echo "rendered $(RENDER)"

kubeconform: template
	# The umbrella now renders the control-plane's CRDs (enabled by default);
	# kubeconform's bundled schema set doesn't resolve apiextensions CRDs.
	$(KUBECONFORM) $(RENDER)

# The umbrella keeps control-plane disabled by default, so validate the
# control-plane chart standalone (renders with its own enabled=true default).
template-controlplane: build-deps
	helm template $(CONTROLPLANE) $(CHARTS_DIR)/$(CONTROLPLANE) > $(RENDER_CP)
	@echo "rendered $(RENDER_CP)"

kubeconform-controlplane: template-controlplane
	# The chart renders a CRD (kubebuilder-generated, sourced verbatim from the
	# control-plane component); kubeconform's bundled schema set does not resolve the
	# apiextensions CRD schema, so ignore missing schemas rather than failing.
	$(KUBECONFORM) $(RENDER_CP)

# Static check with internal TLS on (values-tls.yaml flips global.internalTLS).
# Catches conditional/render errors in the TLS-on path that the default
# (plaintext) `make check` doesn't exercise. cert-manager Certificate/
# ClusterIssuer CRs are ignored (no bundled schema), like the CRDs above.
template-tls: build-deps
	helm template $(UMBRELLA) $(CHARTS_DIR)/$(UMBRELLA) -f values-tls.yaml > $(RENDER_TLS)
	@echo "rendered $(RENDER_TLS)"

kubeconform-tls: template-tls
	$(KUBECONFORM) $(RENDER_TLS)

check-certificate-substrate: build-deps
	./scripts/check-certificate-substrate.sh

check-lvm-storage-substrate: build-deps
	./scripts/check-lvm-storage-substrate.sh

check-service-selectors:
	./scripts/check-service-selectors.sh

check-remote-image-digests: build-deps
	./scripts/check-remote-image-digests.sh

check-redis-exporter-auth:
	./scripts/check-redis-exporter-auth.sh

check-artifact-config:
	./scripts/check-artifact-config.sh

check-tool-runner:
	./scripts/check-tool-runner.sh

check-version-links:
	./scripts/check-version-links.sh

check-manager-contract:
	./scripts/check-manager-contract.sh

check-workload-ca:
	./scripts/check-workload-ca.sh

check-auth-config: build-deps
	./scripts/check-auth-config.sh

check-dup-yaml-keys: build-deps
	./scripts/check-dup-yaml-keys.sh

check-metrics-contract:
	./scripts/check-metrics-contract.sh

check-alerts: build-deps
	./scripts/check-alerts.sh

test-alert-rules: build-deps
	./scripts/test-alert-rules.sh

check-dashboards: build-deps
	./scripts/check-dashboards.sh

check: test-build-dependency-retry lint kubeconform kubeconform-controlplane kubeconform-observability check-internal-observability check-private-control-plane check-certificate-substrate check-lvm-storage-substrate check-service-selectors check-remote-image-digests check-redis-exporter-auth check-artifact-config check-tool-runner check-manager-contract check-workload-ca check-auth-config check-dup-yaml-keys check-metrics-contract check-version-links check-alerts check-dashboards

check-tls: kubeconform-tls kubeconform-observability-tls check-internal-observability check-private-control-plane check-redis-exporter-auth check-observability-tls

# Static check with the observability stack + every component's metrics on
# (values-observability.yaml). Catches conditional/render errors in the
# ServiceMonitor / PodMonitor / PrometheusRule / exporter paths that the
# default (stack off) `make check` doesn't exercise. The Prometheus Operator
# CRDs (ServiceMonitor / PodMonitor / PrometheusRule) have no bundled schema, so
# ignore missing schemas like the platform CRDs above.
template-observability: build-deps
	helm template $(UMBRELLA) $(CHARTS_DIR)/$(UMBRELLA) -f values-observability.yaml > $(RENDER_OBS)
	@echo "rendered $(RENDER_OBS)"

kubeconform-observability: template-observability
	$(KUBECONFORM) $(RENDER_OBS)

check-observability: kubeconform-observability

# Combined static path for the TLS-on observability wiring that neither preset
# exercises alone.
template-observability-tls: build-deps
	helm template $(TLS_TEST_RELEASE) $(CHARTS_DIR)/$(UMBRELLA) --namespace $(TLS_TEST_NAMESPACE) -f values-observability.yaml -f values-tls.yaml > $(RENDER_OBS_TLS)
	@echo "rendered $(RENDER_OBS_TLS)"

kubeconform-observability-tls: template-observability-tls
	$(KUBECONFORM) $(RENDER_OBS_TLS)

check-observability-tls: template-observability-tls
	./scripts/check-observability-tls.sh $(RENDER_OBS_TLS) $(TLS_TEST_RELEASE) $(TLS_TEST_NAMESPACE)

# Class-isolated private observability ingress: a public IPv6 ingress remains on
# the default class while a second IPv4 controller consumes an explicitly named,
# non-auto-assigned MetalLB pool. The same render proves non-proxied DNS,
# edge-certificate, backend-TLS, and direct API contracts.
template-internal-observability: build-deps
	helm template $(TLS_TEST_RELEASE) $(CHARTS_DIR)/$(UMBRELLA) --namespace $(TLS_TEST_NAMESPACE) -f values-observability.yaml -f values-tls.yaml -f values-internal-observability.yaml > $(RENDER_INTERNAL_OBS)
	@echo "rendered $(RENDER_INTERNAL_OBS)"

kubeconform-internal-observability: template-internal-observability
	$(KUBECONFORM) $(RENDER_INTERNAL_OBS)

check-internal-observability: kubeconform-internal-observability
	./scripts/check-internal-observability-ingress.sh $(RENDER_INTERNAL_OBS) $(TLS_TEST_RELEASE) $(TLS_TEST_NAMESPACE)

# Complete same-origin control-plane surface on a private ingress class. This
# proves distinct edge/backend certificate Secrets, verified internal-CA HTTPS,
# non-proxied private DNS, and isolation from the public inference controller.
template-private-control-plane: build-deps
	helm template $(TLS_TEST_RELEASE) $(CHARTS_DIR)/$(UMBRELLA) --namespace $(TLS_TEST_NAMESPACE) -f values-tls.yaml -f values-private-control-plane.yaml > $(RENDER_PRIVATE_CP)
	@echo "rendered $(RENDER_PRIVATE_CP)"

kubeconform-private-control-plane: template-private-control-plane
	$(KUBECONFORM) $(RENDER_PRIVATE_CP)

check-private-control-plane: kubeconform-private-control-plane
	./scripts/check-private-control-plane-ingress.sh $(RENDER_PRIVATE_CP) $(TLS_TEST_RELEASE) $(TLS_TEST_NAMESPACE)

clean:
	rm -f $(RENDER) $(RENDER_CP) $(RENDER_TLS) $(RENDER_OBS) $(RENDER_OBS_TLS) $(RENDER_INTERNAL_OBS) $(RENDER_PRIVATE_CP)
	rm -rf $(CHARTS_DIR)/$(UMBRELLA)/charts $(CHARTS_DIR)/$(CERT_MANAGER_SUBSTRATE)/charts $(CHARTS_DIR)/$(LVM_STORAGE_SUBSTRATE)/charts $(CHARTS_DIR)/$(CONTROLPLANE)/charts $(CHARTS_DIR)/$(OBSERVABILITY)/charts
