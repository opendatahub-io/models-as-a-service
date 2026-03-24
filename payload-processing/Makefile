# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION = 1.31.0
# Set COVERAGE=true or COVERAGE=1 on make test-unit to print per-func coverage (cover.out is removed after).
COVERAGE ?=

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

GIT_COMMIT_SHA ?= "$(shell git rev-parse HEAD 2>/dev/null)"
GIT_TAG ?= $(shell git describe --tags --dirty --always)
TARGETARCH ?= $(shell go env GOARCH)
PLATFORMS ?= linux/$(TARGETARCH)
DOCKER_BUILDX_CMD ?= docker buildx
IMAGE_BUILD_CMD ?= $(DOCKER_BUILDX_CMD) build
IMAGE_BUILD_EXTRA_OPTS ?=

IMAGE_REGISTRY ?= quay.io/opendatahub-io
IMAGE_NAME := ai-gateway-payload-processing
IMAGE_REPO ?= $(IMAGE_REGISTRY)/$(IMAGE_NAME)
IMAGE_TAG ?= $(IMAGE_REPO):$(GIT_TAG)

BASE_IMAGE ?= gcr.io/distroless/static:nonroot
BUILDER_IMAGE ?= golang:1.25
ifdef GO_VERSION
BUILDER_IMAGE = golang:$(GO_VERSION)
endif

BUILD_REF ?= $(shell git describe --abbrev=0 2>/dev/null)
ifdef EXTRA_TAG
IMAGE_EXTRA_TAG ?= $(IMAGE_REPO):$(EXTRA_TAG)
BUILD_REF = $(EXTRA_TAG)
endif
ifdef IMAGE_EXTRA_TAG
IMAGE_BUILD_EXTRA_OPTS += -t $(IMAGE_EXTRA_TAG)
endif

# The name of the kind cluster to use for the "kind-load" target.
KIND_CLUSTER ?= kind

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
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...


.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run --timeout 5m

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: tidy
tidy: ## Run go work sync (if go.work exists) and go mod tidy per module.
	@if [ -f go.work ]; then go work sync; fi
	find . -name go.mod -execdir sh -c 'go mod tidy' \;

.PHONY: verify
verify: tidy vet fmt lint  ## Verify the codebase (tidy, vet, fmt, lint).

.PHONY: test-unit
test-unit: envtest ## Run unit tests. Optional: COVERAGE=true (or 1) for go tool cover summary.
	@set -e; \
	kubebuilder_assets_path="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)"; \
	if [ "$(COVERAGE)" = "true" ] || [ "$(COVERAGE)" = "1" ]; then \
		CGO_ENABLED=1 KUBEBUILDER_ASSETS="$$kubebuilder_assets_path" go test ./pkg/... -race -count=1 -coverprofile=cover.out; \
		go tool cover -func=cover.out; \
		rm -f cover.out; \
	else \
		CGO_ENABLED=1 KUBEBUILDER_ASSETS="$$kubebuilder_assets_path" go test ./pkg/... -race -count=1; \
	fi

.PHONY: test
test: test-unit ## Run unit tests (alias for test-unit).

##@ Build

# Build the container image
.PHONY: image-local-build
image-local-build: ## Build the image using Docker Buildx for local development.
	set -e; \
	builder=$$($(DOCKER_BUILDX_CMD) create --use); \
	trap '$(DOCKER_BUILDX_CMD) rm -f "$$builder"' EXIT; \
	$(MAKE) image-build PUSH="$(PUSH)" LOAD="$(LOAD)"

.PHONY: image-local-push
image-local-push: PUSH=--push ## Build the image for local development and push it to $IMAGE_REPO.
image-local-push: image-local-build

.PHONY: image-local-load
image-local-load: LOAD=--load ## Build the image for local development and load it in the local Docker registry.
image-local-load: image-local-build

.PHONY: image-build
image-build: ## Build the image using Docker Buildx.
	$(IMAGE_BUILD_CMD) -t $(IMAGE_TAG) \
		--platform=$(PLATFORMS) \
		--build-arg BASE_IMAGE=$(BASE_IMAGE) \
		--build-arg BUILDER_IMAGE=$(BUILDER_IMAGE) \
		--build-arg COMMIT_SHA=${GIT_COMMIT_SHA} \
		--build-arg BUILD_REF=${BUILD_REF} \
		$(PUSH) \
		$(LOAD) \
		$(IMAGE_BUILD_EXTRA_OPTS) ./

.PHONY: image-push
image-push: PUSH=--push ## Build the image and push it to $IMAGE_REPO.
image-push: image-build

.PHONY: image-load
image-load: LOAD=--load ## Build the image and load it in the local Docker registry.
image-load: image-build

.PHONY: image-kind
image-kind: image-build ## Build the image and load it to kind cluster $KIND_CLUSTER ("kind" by default).
	kind load docker-image $(IMAGE_TAG) --name $(KIND_CLUSTER)

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	[ -d $@ ] || mkdir -p $@

ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

ENVTEST_VERSION ?= release-0.19
GOLANGCI_LINT_VERSION ?= v2.9.0

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef
