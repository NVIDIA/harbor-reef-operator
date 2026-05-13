CHAINSAW_VERSION ?= v0.2.15
CHAINSAW := $(shell command -v chainsaw 2>/dev/null)
TEST_DIR := test/chainsaw
EXEC_TIMEOUT ?= 60s
CHAINSAW_ENV ?= .chainsaw.env

# Public-safe defaults. Override by exporting these in the shell, by
# populating .chainsaw.env (gitignored), or by the CI runner pulling
# secrets from a vault. The values shipped here will not work against any
# real cluster on purpose -- failing loudly is preferred over hitting a
# stale internal target.
HARBOR_API_BASE     ?= https://harbor.example.com
HARBOR_API_HOST     ?= harbor.example.com
HARBOR_ADMIN_NS     ?= harbor-reef
HARBOR_ADMIN_SECRET ?= harbor-admin-password
HARBOR_ADMIN_KEY    ?= HARBOR_ADMIN_PASSWORD

.PHONY: help
help:
	@echo "Targets:"
	@echo "  test             Run Go unit tests"
	@echo "  chainsaw         Run end-to-end chainsaw tests against the current kube context"
	@echo "  chainsaw-install Install chainsaw $(CHAINSAW_VERSION) to ~/.local/bin"
	@echo ""
	@echo "Environment for 'chainsaw' (override or set in $(CHAINSAW_ENV)):"
	@echo "  HARBOR_API_BASE  Full Harbor URL with scheme (default: $(HARBOR_API_BASE))"
	@echo "  HARBOR_API_HOST  Harbor host only, no scheme  (default: $(HARBOR_API_HOST))"
	@echo "  HARBOR_ADMIN_PASS Required. Plaintext Harbor admin password."
	@echo "                   If unset, the Makefile fetches it from the in-cluster"
	@echo "                   admin secret ($(HARBOR_ADMIN_NS)/$(HARBOR_ADMIN_SECRET))."

.PHONY: test
test:
	go test -v ./...

.PHONY: chainsaw-install
chainsaw-install:
	@mkdir -p $(HOME)/.local/bin
	@OS=$$(uname -s | tr '[:upper:]' '[:lower:]'); \
	  ARCH=$$(uname -m); \
	  case "$$ARCH" in x86_64) ARCH=amd64;; aarch64|arm64) ARCH=arm64;; esac; \
	  URL="https://github.com/kyverno/chainsaw/releases/download/$(CHAINSAW_VERSION)/chainsaw_$${OS}_$${ARCH}.tar.gz"; \
	  echo "Downloading $$URL"; \
	  curl -sSL "$$URL" | tar -xz -C $(HOME)/.local/bin chainsaw; \
	  chmod +x $(HOME)/.local/bin/chainsaw
	@echo "Installed chainsaw to $(HOME)/.local/bin/chainsaw"
	@$(HOME)/.local/bin/chainsaw version

.PHONY: chainsaw
chainsaw:
ifndef CHAINSAW
	$(error chainsaw is not installed. Run 'make chainsaw-install' or set PATH to include it.)
endif
	@set -e; \
	if [ -f "$(CHAINSAW_ENV)" ]; then \
	  echo "Sourcing $(CHAINSAW_ENV)"; \
	  set -a; . ./$(CHAINSAW_ENV); set +a; \
	fi; \
	: $${HARBOR_API_BASE:=$(HARBOR_API_BASE)}; \
	: $${HARBOR_API_HOST:=$(HARBOR_API_HOST)}; \
	if [ -z "$$HARBOR_ADMIN_PASS" ]; then \
	  echo "HARBOR_ADMIN_PASS not set; fetching from $(HARBOR_ADMIN_NS)/$(HARBOR_ADMIN_SECRET)"; \
	  HARBOR_ADMIN_PASS=$$(kubectl -n $(HARBOR_ADMIN_NS) get secret $(HARBOR_ADMIN_SECRET) -o jsonpath='{.data.$(HARBOR_ADMIN_KEY)}' | base64 -d); \
	fi; \
	export HARBOR_API_BASE HARBOR_API_HOST HARBOR_ADMIN_PASS; \
	echo "Running chainsaw against $$HARBOR_API_BASE"; \
	$(CHAINSAW) test $(TEST_DIR) --exec-timeout $(EXEC_TIMEOUT)
