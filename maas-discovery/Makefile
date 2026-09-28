# MaaS Discovery Makefile

##@ General

.PHONY: help
help: ##	Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
	@echo ""
	@echo "\033[1mOptional build arguments\033[0m (e.g. \033[36mmake build GO_STRICTFIPS=true\033[0m):"
	@echo "  \033[36mGO_STRICTFIPS=true\033[0m  strict FIPS runtime for \033[36mbuild\033[0m / \033[36mrun\033[0m / \033[36mbuild-image\033[0m / \033[36mbuild-image-konflux\033[0m"

PROJECT_DIR := $(shell dirname $(abspath $(lastword $(MAKEFILE_LIST))))

include tools.mk

BINARY_NAME := discovery
BUILD_DIR := $(PROJECT_DIR)/bin

# Go build environment (override on the CLI, e.g. make build GO_STRICTFIPS=true).
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

##@ Development

.PHONY: build
build: tidy lint test binary ##	run full build: tidy, lint, test, binary

.PHONY: binary
binary: $(BUILD_DIR) ##	build discovery binary to bin/discovery (skip checks)
	$(GO_ENV) go build -o "$(BUILD_DIR)/$(BINARY_NAME)" ./cmd/discovery

$(BUILD_DIR):
	mkdir -p "$(BUILD_DIR)"

.PHONY: run
run: binary ##	build and run discovery locally (self-signed TLS)
	"$(BUILD_DIR)/$(BINARY_NAME)" --self-signed

TEST_FLAGS ?= -race -coverprofile=coverage.out
.PHONY: test
test: tidy ##	run tests with coverage
	go test $(TEST_FLAGS) ./...
	@go tool cover -html=coverage.out -o coverage.html
	@echo "Test coverage report generated: $(abspath coverage.html)"

.PHONY: tidy
tidy:
	go mod tidy

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

##@ Deployment

## Container image
include container.mk
