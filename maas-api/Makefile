PROJECT_DIR:=$(shell dirname $(abspath $(lastword $(MAKEFILE_LIST))))

BINARY_NAME := maas-api
BUILD_DIR := $(PROJECT_DIR)/bin

include tools.mk

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

GIT_COMMIT := $(shell git rev-parse --is-inside-work-tree >/dev/null 2>&1 && git rev-parse --short HEAD)
GIT_BRANCH := $(shell git rev-parse --is-inside-work-tree >/dev/null 2>&1 && git rev-parse --abbrev-ref HEAD)
BUILD_TIME := $(shell date -u '+%Y-%m-%d_%H:%M:%S')

# Ldflags for build info
LDFLAGS ?= -ldflags "-s -w -X main.version=$(TAG) -X main.commit=$(GIT_COMMIT) -X main.buildTime=$(BUILD_TIME)"

.PHONY: help
help: ## Show this help message
	@echo "MaaS API Makefile"
	@echo ""
	@echo "Usage: make <target> [REPO=your-repo] [TAG=your-tag]"
	@echo ""
	@echo "Examples:"
	@echo "  make build-image REPO=ghcr.io/myorg/maas-api TAG=v1.0.0"
	@echo "  make push-image REPO=ghcr.io/myorg/maas-api TAG=latest"
	@echo ""
	@echo "Targets:"
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

LINT_FIX ?= false
.PHONY: lint
lint: $(GOLANGCI_LINT) ## Executes golangci linters (use LINT_FIX=true to fix lint issues)
	$(GOLANGCI_LINT) fmt
	$(GOLANGCI_LINT) run $(if $(filter true,$(LINT_FIX)),--fix)

.PHONY: tidy
tidy: ## Tidy Go modules
	@echo "Tidying Go modules..."
	go mod tidy
	go mod verify

.PHONY: deps
deps: ## Download Go dependencies
	@echo "Downloading Go dependencies..."
	go mod download

.PHONY: build
build: deps lint test binary ## Build the maas-api binary

.PHONY: binary
binary: $(BUILD_DIR)
	$(GO_ENV) go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/

$(BUILD_DIR):
	mkdir -p $(BUILD_DIR)

SRC_DIRS := $(PROJECT_DIR)/cmd $(PROJECT_DIR)/internal
PKGS := $(foreach d,$(SRC_DIRS),$(d)/...)
TEST_FLAGS ?= -race -coverprofile=coverage.out
.PHONY: test
test: ## Run Go tests
	@echo "Running tests in..."
	go test -v $(TEST_FLAGS) $(PKGS)
	@go tool cover -html=coverage.out -o coverage.html
	@echo "Test coverage report generated: $(abspath coverage.html)"

## Container image
include container.mk

## Deployment

PRE_DEPLOY_STEP ?=
CORE_KUSTOMIZATION := $(abspath $(PROJECT_DIR)/../deployment/base/maas-api/core)

define deploy
	kubectl create namespace maas-api || true
	cd $(CORE_KUSTOMIZATION) && \
		set -eu; \
		cp kustomization.yaml kustomization.yaml.backup && \
		trap 'mv $(CORE_KUSTOMIZATION)/kustomization.yaml.backup $(CORE_KUSTOMIZATION)/kustomization.yaml 2>/dev/null || true' EXIT INT TERM && \
		kustomize edit set image maas-api=$(FULL_IMAGE) && \
		cd $(PROJECT_DIR)/deploy/$(1) && \
		$(if $(PRE_DEPLOY_STEP),$(PRE_DEPLOY_STEP) &&) \
		kustomize build . --load-restrictor LoadRestrictionsNone | kubectl apply -f -
endef

.PHONY: deploy-dev
deploy-dev: ## Deploy development version
	$(call deploy,overlays/dev)

default_run_flags := --debug
RUN_FLAGS ?= 
.PHONY: run
run: build ## Run the maas-api locally
	$(BUILD_DIR)/$(BINARY_NAME) $(default_run_flags) $(RUN_FLAGS) 

.PHONY: version
version: ## Show version information
	@echo "Image: $(FULL_IMAGE)"
	@echo "Git Commit: $(GIT_COMMIT)"
	@echo "Git Branch: $(GIT_BRANCH)"
	@echo "Build Time: $(BUILD_TIME)"

# Default target
.DEFAULT_GOAL := help
