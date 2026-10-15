SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c

TERRAFORM ?= terraform
TFLINT ?= tflint
PYTHON ?= python3
STACKS := hosted-aws/bootstrap hosted-aws/baseline
# Forge's per-customer module (HOR-593) is validated with the same pinned
# toolchain; it is owned by forge/ and embedded in the forge binary.
MODULES := ../forge/internal/hosted/module

.PHONY: check fmt-check validate test lint

check: fmt-check validate test lint

fmt-check:
	$(TERRAFORM) fmt -check -recursive -diff .
	@for module in $(MODULES); do $(TERRAFORM) fmt -check -recursive -diff "$$module"; done

# No backend and no credentials: providers come from the committed lock files.
validate:
	@for stack in $(STACKS) $(MODULES); do \
		echo ":: terraform validate $$stack"; \
		$(TERRAFORM) -chdir="$$stack" init -backend=false -input=false -lockfile=readonly >/dev/null; \
		$(TERRAFORM) -chdir="$$stack" validate; \
	done

# Source-contract guards and mocked plans: no state or cloud credentials.
test: validate
	$(PYTHON) -B -m unittest discover -s tests -v
	$(TERRAFORM) -chdir=hosted-aws/baseline test -no-color

lint:
	@for stack in $(STACKS) $(MODULES); do \
		echo ":: tflint $$stack"; \
		$(TFLINT) --chdir="$$stack" --config="$(CURDIR)/.tflint.hcl"; \
	done
