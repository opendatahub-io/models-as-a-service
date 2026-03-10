# MaaS Controller Makefile
# Build and deploy the MaaS control plane (MaaSModelRef, MaaSAuthPolicy, MaaSSubscription)

.PHONY: build run test tidy install uninstall image-build image-push docker-build generate manifests verify-codegen help

help:
	@echo "MaaS Controller make targets:"
	@echo "  make build        - build manager binary to bin/manager"
	@echo "  make run          - build and run manager locally"
	@echo "  make test         - run tests"
	@echo "  make generate     - generate deepcopy code for API types"
	@echo "  make manifests    - generate CRD manifests"
	@echo "  make install      - apply deployment/base/maas-controller/default (CRDs, RBAC, deployment)"
	@echo "  make uninstall    - delete deployment/base/maas-controller/default resources"
	@echo "  make verify-codegen - run generate + manifests and fail if files changed (CI check)"
	@echo "  make image-build  - build container image (podman/buildah/docker); default IMAGE=quay.io/opendatahub/maas-controller"
	@echo "  make image-push   - build and push image; override with IMAGE=... IMAGE_TAG=..."

CONTROLLER_GEN_VERSION ?= v0.16.4
CONTROLLER_GEN = $(BUILD_DIR)/controller-gen

BINARY_NAME := manager
BUILD_DIR := bin

# Container build: use podman, buildah, or docker (default: podman if available, else buildah, else docker)
CONTAINER_RUNTIME := $(shell command -v podman 1>/dev/null 2>&1 && echo podman || (command -v buildah 1>/dev/null 2>&1 && echo buildah || echo docker))
# Default image
IMAGE ?= quay.io/opendatahub/maas-controller
IMAGE_TAG ?= latest
FULL_IMAGE := $(IMAGE):$(IMAGE_TAG)

build: tidy $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/manager

$(BUILD_DIR):
	mkdir -p $(BUILD_DIR)

run: build
	./$(BUILD_DIR)/$(BINARY_NAME)

test: tidy
	go test ./...

tidy:
	go mod tidy

# Install controller-gen into local bin (pinned version)
$(CONTROLLER_GEN): | $(BUILD_DIR)
	GOBIN=$(abspath $(BUILD_DIR)) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

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

# Build container image (podman, buildah, or docker)
# buildah uses "bud" (build-using-dockerfile) instead of "build"
BUILD_CMD := $(if $(filter buildah,$(CONTAINER_RUNTIME)),bud,build)
image-build:
	$(CONTAINER_RUNTIME) $(BUILD_CMD) -t $(FULL_IMAGE) --platform=linux/amd64 -f Dockerfile .
	@echo "Built $(FULL_IMAGE) with $(CONTAINER_RUNTIME)"

# Alias for image-build (backward compatibility)
docker-build: image-build

# Push image to registry (must be logged in, e.g. podman login quay.io)
image-push: image-build
	$(CONTAINER_RUNTIME) push $(FULL_IMAGE)
	@echo "Pushed $(FULL_IMAGE)"
