# Doory backend -> Go port. Gate ladder mirrors PiG: generate -> check -> verify.
#
# Override the Go toolchain with:  make check GO=go1.27

GO ?= go
UPSTREAM_COMMIT = $(shell sed -n 's/.*UpstreamCommit = "\([0-9a-f]*\)".*/\1/p' internal/dooryversion/dooryversion.go)

.DEFAULT_GOAL := help
.PHONY: help upstream-mirror generate check verify parity coverage port-map-drift lint-scenarios divergence-consistency parity-fast

help: ## List the available targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

upstream-mirror: ## Refresh .upstream/current to the pinned commit (pristine, detached)
	@test -d .upstream/current || git clone https://github.com/shaongitbd/doory .upstream/current
	git -C .upstream/current fetch --quiet origin
	git -C .upstream/current checkout --quiet --detach $(UPSTREAM_COMMIT)
	git -C .upstream/current status --porcelain | grep . && { echo "mirror not pristine"; exit 1; } || true

generate: ## Regenerate compiler-derived inventories (never hand-edit outputs)
	@echo "TODO(M0): ledgergen introspection against the pinned oracle" && exit 1

check: ## Build, vet, lint, unit tests, drift gates, divergence guards, parity-fast
	$(GO) build ./...
	$(GO) vet ./...
	$(GO) test ./...
	@$(MAKE) port-map-drift
	@$(MAKE) divergence-consistency

verify: check ## check + full parity + contracts
	@echo "TODO(M1): full parity" && exit 1

parity: ## Parallel oracle/Go scenario pairs
	@echo "TODO(M1): parity runner" && exit 1

parity-fast: ## One strict oracle/Go pair per hermetic scenario
	@echo "TODO(M1): parity runner" && exit 1

coverage: ## Regenerate the coverage report embedded in AGENTS.md
	@echo "TODO(M1): coverage tool" && exit 1

port-map-drift: ## Reject unmapped Go sources and rows whose upstream file vanished
	@echo "TODO(M0): port-map drift gate" && exit 1

lint-scenarios: ## Validate scenario TOMLs (normalize reasons, comparator rules)
	@echo "TODO(M0): scenario linter" && exit 1

divergence-consistency: ## Match DIVERGENCES records to call-site markers
	@echo "TODO(M0): divergence ledger" && exit 1
