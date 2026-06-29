# Weight Controller Makefile
# Build and deploy the Weight Controller for multi-cluster routing

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

PROJECT_DIR := $(shell dirname $(abspath $(lastword $(MAKEFILE_LIST))))

BINARY_NAME := weight-controller
BUILD_DIR := $(PROJECT_DIR)/bin

# Go build environment
GOOS   ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
CGO_ENABLED ?= 0

GO_ENV := GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=$(CGO_ENABLED)

# Image configuration
IMG_REGISTRY ?= quay.io/opendatahub
IMG_NAME ?= weight-controller
IMG_TAG ?= latest
IMG ?= $(IMG_REGISTRY)/$(IMG_NAME):$(IMG_TAG)

# Golangci-lint
GOLANGCI_LINT_VERSION ?= v2.6.2
GOLANGCI_LINT = $(BUILD_DIR)/golangci-lint

##@ Development

.PHONY: build
build: tidy generate lint test binary ## Run full build: tidy, generate, lint, test, binary

.PHONY: binary
binary: $(BUILD_DIR) ## Build controller binary to bin/weight-controller
	$(GO_ENV) go build -trimpath -ldflags="-s -w" -o "$(BUILD_DIR)/$(BINARY_NAME)" ./cmd

$(BUILD_DIR):
	mkdir -p "$(BUILD_DIR)"

.PHONY: run
run: binary ## Build and run controller locally
	"$(BUILD_DIR)/$(BINARY_NAME)"

TEST_FLAGS ?= -race -coverprofile=coverage.out
.PHONY: test
test: tidy ## Run tests with coverage
	go test $(TEST_FLAGS) ./...
	@go tool cover -html=coverage.out -o coverage.html
	@echo "Test coverage report generated: $(abspath coverage.html)"

.PHONY: tidy
tidy: ## Run go mod tidy
	go mod tidy

.PHONY: generate
generate: ## Generate deepcopy code for API types
	go generate ./...

$(GOLANGCI_LINT): | $(BUILD_DIR)
	GOBIN=$(abspath $(BUILD_DIR)) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

LINT_FIX ?= false
.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run golangci-lint (use LINT_FIX=true to fix lint issues)
	"$(GOLANGCI_LINT)" fmt $(if $(filter true,$(LINT_FIX)),,--diff)
	"$(GOLANGCI_LINT)" run $(if $(filter true,$(LINT_FIX)),--fix)

##@ Container Image

.PHONY: docker-build
docker-build: ## Build container image (runs from repo root)
	docker build -t $(IMG) -f $(PROJECT_DIR)/Dockerfile $(PROJECT_DIR)/..

.PHONY: docker-push
docker-push: ## Push container image
	docker push $(IMG)

.PHONY: docker-build-push
docker-build-push: docker-build docker-push ## Build and push container image

##@ Deployment

.PHONY: install
install: ## Apply deployment/base/weight-controller/default (RBAC, deployment)
	kubectl apply -k ../deployment/base/weight-controller/default

.PHONY: uninstall
uninstall: ## Delete deployment/base/weight-controller/default resources
	kubectl delete -k ../deployment/base/weight-controller/default --ignore-not-found

##@ Clean

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BUILD_DIR)
	rm -f coverage.out coverage.html
