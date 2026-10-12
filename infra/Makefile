SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c

TERRAFORM ?= terraform
TFLINT ?= tflint
STACKS := hosted-aws/bootstrap hosted-aws/baseline

.PHONY: check fmt-check validate lint

check: fmt-check validate lint

fmt-check:
	$(TERRAFORM) fmt -check -recursive -diff .

# No backend and no credentials: providers come from the committed lock files.
validate:
	@for stack in $(STACKS); do \
		echo ":: terraform validate $$stack"; \
		$(TERRAFORM) -chdir="$$stack" init -backend=false -input=false -lockfile=readonly >/dev/null; \
		$(TERRAFORM) -chdir="$$stack" validate; \
	done

lint:
	@for stack in $(STACKS); do \
		echo ":: tflint $$stack"; \
		$(TFLINT) --chdir="$$stack" --config="$(CURDIR)/.tflint.hcl"; \
	done
