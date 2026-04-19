# MaaS Controller Makefile
# Build and deploy the MaaS control plane (MaaSModelRef, MaaSAuthPolicy, MaaSSubscription)

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ##	Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
	@echo ""
	@echo "\033[1mOptional build arguments\033[0m (e.g. \033[36mmake build GO_STRICTFIPS=true\033[0m):"
	@echo "  \033[36mGO_STRICTFIPS=true\033[0m  strict FIPS runtime for \033[36mbuild\033[0m / \033[36mrun\033[0m / \033[36mbuild-image\033[0m / \033[36mbuild-image-konflux\033[0m"

PROJECT_DIR := $(shell dirname $(abspath $(lastword $(MAKEFILE_LIST))))

include tools.mk

BINARY_NAME := manager
BUILD_DIR := $(PROJECT_DIR)/bin

# Go build environment (override on the CLI, e.g. make build GO_STRICTFIPS=true).
# Strict FIPS uses GOEXPERIMENT=strictfipsruntime; keep CGO_ENABLED=1 for that path.
GOOS   ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
GO_STRICTFIPS ?= false

CGO_ENABLED ?= 1

ifeq ($(GO_STRICTFIPS),true)
  GOEXPERIMENT ?= strictfipsruntime
endif

GO_ENV := GOOS=$(GOOS) GOARCH=$(GOARCH)
ifdef GOEXPERIMENT
  GO_ENV += GOEXPERIMENT=$(GOEXPERIMENT)
endif
ifdef CGO_ENABLED
  GO_ENV += CGO_ENABLED=$(CGO_ENABLED)
endif

CONTROLLER_GEN_VERSION ?= v0.16.4
CONTROLLER_GEN = $(BUILD_DIR)/controller-gen

##@ Development

.PHONY: build
build: tidy generate manifests lint test binary ##	run full build: tidy, generate, manifests, lint, test, binary

.PHONY: binary
binary: $(BUILD_DIR) ##	build manager binary to bin/manager (skip checks)
	$(GO_ENV) go build -o "$(BUILD_DIR)/$(BINARY_NAME)" ./cmd/manager

$(BUILD_DIR):
	mkdir -p "$(BUILD_DIR)"

.PHONY: run
run: binary ##	build and run manager locally
	"$(BUILD_DIR)/$(BINARY_NAME)"

TEST_FLAGS ?= -race -coverprofile=coverage.out
.PHONY: test
test: tidy ##	run tests with coverage
	go test $(TEST_FLAGS) ./...
	@go tool cover -html=coverage.out -o coverage.html
	@echo "Test coverage report generated: $(abspath coverage.html)"

.PHONY: tidy
tidy:
	go mod tidy

# Install controller-gen into local bin (pinned version)
$(CONTROLLER_GEN): | $(BUILD_DIR)
	GOBIN=$(abspath $(BUILD_DIR)) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

LINT_FIX ?= false
.PHONY: lint
lint: $(GOLANGCI_LINT) ##	run golangci-lint (use LINT_FIX=true to fix lint issues)
ifeq ($(LINT_FIX),true)
	"$(GOLANGCI_LINT)" fmt
	"$(GOLANGCI_LINT)" run --fix
else
	"$(GOLANGCI_LINT)" fmt --diff
	"$(GOLANGCI_LINT)" run
endif

.PHONY: generate
generate: $(CONTROLLER_GEN) ##	generate deepcopy code for API types
	$(CONTROLLER_GEN) object paths="./api/..."

.PHONY: manifests
manifests: $(CONTROLLER_GEN) ##	generate CRD and RBAC manifests
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:dir=../deployment/base/maas-controller/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=maas-controller-role paths="./..." output:rbac:dir=../deployment/base/maas-controller/rbac
	@mv ../deployment/base/maas-controller/rbac/role.yaml ../deployment/base/maas-controller/rbac/clusterrole.yaml

.PHONY: verify-codegen
verify-codegen: generate manifests ##	run generate + manifests and fail if files changed (CI check)
	@if ! git diff --quiet HEAD -- api/ ../deployment/base/maas-controller/crd/bases/ ../deployment/base/maas-controller/rbac/clusterrole.yaml; then \
		echo "ERROR: Generated files are out of date. Please run 'make -C maas-controller generate manifests' from the repo root and commit the results."; \
		git diff --stat HEAD -- api/ ../deployment/base/maas-controller/crd/bases/ ../deployment/base/maas-controller/rbac/clusterrole.yaml; \
		exit 1; \
	fi
	@UNTRACKED=$$(git ls-files --others --exclude-standard -- api/ ../deployment/base/maas-controller/crd/bases/ ../deployment/base/maas-controller/rbac/); \
	if [ -n "$$UNTRACKED" ]; then \
		echo "ERROR: Untracked generated files detected. Please commit them:"; \
		echo "$$UNTRACKED"; \
		exit 1; \
	fi
	@echo "Generated files are up to date."

##@ Deployment

# Install CRDs, RBAC, and manager deployment (default: opendatahub namespace)
.PHONY: install
install: ##	apply deployment/base/maas-controller/default (CRDs, RBAC, deployment)
	kubectl apply -k ../deployment/base/maas-controller/default

.PHONY: uninstall
uninstall: ##	delete deployment/base/maas-controller/default resources
	kubectl delete -k ../deployment/base/maas-controller/default --ignore-not-found

## Container image
include container.mk
