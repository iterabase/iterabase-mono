SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c

TERRAFORM ?= terraform
TFLINT ?= tflint
PYTHON ?= python3
STACKS := hosted-aws/bootstrap hosted-aws/baseline

.PHONY: check fmt-check validate test lint

check: fmt-check validate test lint

fmt-check:
	$(TERRAFORM) fmt -check -recursive -diff .

# No backend and no credentials: providers come from the committed lock files.
validate:
	@for stack in $(STACKS); do \
		echo ":: terraform validate $$stack"; \
		$(TERRAFORM) -chdir="$$stack" init -backend=false -input=false -lockfile=readonly >/dev/null; \
		$(TERRAFORM) -chdir="$$stack" validate; \
	done

# Source-contract guards and mocked plans: no state or cloud credentials.
test: validate
	$(PYTHON) -B -m unittest discover -s tests -v
	$(TERRAFORM) -chdir=hosted-aws/baseline test -no-color

lint:
	@for stack in $(STACKS); do \
		echo ":: tflint $$stack"; \
		$(TFLINT) --chdir="$$stack" --config="$(CURDIR)/.tflint.hcl"; \
	done
