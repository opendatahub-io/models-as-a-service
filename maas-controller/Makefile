# MaaS Controller Makefile
# Build and deploy the MaaS control plane (MaaSModelRef, MaaSAuthPolicy, MaaSSubscription)

PROJECT_DIR := $(shell dirname $(abspath $(lastword $(MAKEFILE_LIST))))

include tools.mk

.PHONY: build run test tidy lint install uninstall generate manifests verify-codegen help

help:
	@echo "MaaS Controller make targets:"
	@echo "  make build              - build manager binary to bin/manager"
	@echo "  make run                - build and run manager locally"
	@echo "  make test               - run tests"
	@echo "  make lint               - run golangci-lint"
	@echo "  make generate           - generate deepcopy code for API types"
	@echo "  make manifests          - generate CRD manifests"
	@echo "  make install            - apply deployment/base/maas-controller/default (CRDs, RBAC, deployment)"
	@echo "  make uninstall          - delete deployment/base/maas-controller/default resources"
	@echo "  make verify-codegen     - run generate + manifests and fail if files changed (CI check)"
	@echo "  make build-image        - build container image; override with REPO=... TAG=..."
	@echo "  make build-image-konflux - build container image using Dockerfile.konflux"
	@echo "  make push-image         - push container image to registry"
	@echo ""
	@echo "FIPS options:"
	@echo "  make build GO_STRICTFIPS=true  - build with FIPS strict runtime"

BINARY_NAME := manager
BUILD_DIR := $(PROJECT_DIR)/bin

GOOS   ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
GO_STRICTFIPS ?= false

CGO_ENABLED  ?= 1

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

## Container image
include container.mk

build: tidy $(BUILD_DIR)
	$(GO_ENV) go build -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/manager

$(BUILD_DIR):
	mkdir -p $(BUILD_DIR)

run: build
	$(BUILD_DIR)/$(BINARY_NAME)

test: tidy
	go test ./...

tidy:
	go mod tidy

# Install controller-gen into local bin (pinned version)
$(CONTROLLER_GEN): | $(BUILD_DIR)
	GOBIN=$(abspath $(BUILD_DIR)) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

LINT_FIX ?= false
lint: $(GOLANGCI_LINT) ## Run golangci-lint (use LINT_FIX=true to fix lint issues)
ifeq ($(LINT_FIX),true)
	$(GOLANGCI_LINT) fmt
	$(GOLANGCI_LINT) run --fix
else
	$(GOLANGCI_LINT) fmt --diff
	$(GOLANGCI_LINT) run
endif

# Generate deepcopy code for API types
generate: $(CONTROLLER_GEN)
	$(CONTROLLER_GEN) object paths="./api/..."

# Generate CRD manifests
manifests: $(CONTROLLER_GEN)
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:dir=../deployment/base/maas-controller/crd/bases

# Run generate + manifests and verify no files changed (used by CI)
verify-codegen: generate manifests
	@if ! git diff --quiet HEAD -- api/ ../deployment/base/maas-controller/crd/bases/; then \
		echo "ERROR: Generated files are out of date. Please run 'make generate manifests' and commit the results."; \
		git diff --stat HEAD -- api/ ../deployment/base/maas-controller/crd/bases/; \
		exit 1; \
	fi
	@UNTRACKED=$$(git ls-files --others --exclude-standard -- api/ ../deployment/base/maas-controller/crd/bases/); \
	if [ -n "$$UNTRACKED" ]; then \
		echo "ERROR: Untracked generated files detected. Please commit them:"; \
		echo "$$UNTRACKED"; \
		exit 1; \
	fi
	@echo "Generated files are up to date."

# Install CRDs, RBAC, and manager deployment (default: opendatahub namespace)
install:
	kubectl apply -k ../deployment/base/maas-controller/default

uninstall:
	kubectl delete -k ../deployment/base/maas-controller/default --ignore-not-found