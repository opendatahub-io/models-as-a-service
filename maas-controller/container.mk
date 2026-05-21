## Container image configuration and targets

CONTAINER_ENGINE ?= podman
REPO ?= quay.io/opendatahub/maas-controller
TAG ?= latest
FULL_IMAGE ?= $(REPO):$(TAG)

# Dockerfile COPY paths are relative to the repository root (maas-controller/, maas-api/, deployment/).
# This file is included from maas-controller/Makefile; build context must be the repo parent.
CONTAINER_MK_DIR := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
IMAGE_CONTEXT ?= $(abspath $(CONTAINER_MK_DIR)/..)
CONTROLLER_DOCKERFILE := $(CONTAINER_MK_DIR)Dockerfile
CONTROLLER_DOCKERFILE_KONFLUX := $(CONTAINER_MK_DIR)Dockerfile.konflux

DOCKER_BUILD_ARGS := --build-arg CGO_ENABLED=$(CGO_ENABLED)
ifdef GOEXPERIMENT
  DOCKER_BUILD_ARGS += --build-arg GOEXPERIMENT=$(GOEXPERIMENT)
endif

##@ Build

.PHONY: build-image
build-image: ##	Build container image (use REPO= and TAG= to specify image)
	@echo "Building container image $(FULL_IMAGE)..."
	@echo "  context: $(IMAGE_CONTEXT)"
	@echo "  dockerfile: $(CONTROLLER_DOCKERFILE)"
	$(CONTAINER_ENGINE) build $(DOCKER_BUILD_ARGS) $(CONTAINER_ENGINE_EXTRA_FLAGS) \
		-f "$(CONTROLLER_DOCKERFILE)" -t "$(FULL_IMAGE)" "$(IMAGE_CONTEXT)"
	@echo "Container image $(FULL_IMAGE) built successfully"

.PHONY: build-image-konflux
build-image-konflux: ##	Build container image with Dockerfile.konflux
	@echo "Building container image $(FULL_IMAGE) using Dockerfile.konflux..."
	@echo "  context: $(IMAGE_CONTEXT)"
	@echo "  dockerfile: $(CONTROLLER_DOCKERFILE_KONFLUX)"
	$(CONTAINER_ENGINE) build $(DOCKER_BUILD_ARGS) $(CONTAINER_ENGINE_EXTRA_FLAGS) \
		-f "$(CONTROLLER_DOCKERFILE_KONFLUX)" -t "$(FULL_IMAGE)" "$(IMAGE_CONTEXT)"
	@echo "Container image $(FULL_IMAGE) built successfully"

.PHONY: push-image
push-image: ##	Push container image (use REPO= and TAG= to specify image)
	@echo "Pushing container image $(FULL_IMAGE)..."
	@$(CONTAINER_ENGINE) push "$(FULL_IMAGE)"
	@echo "Container image $(FULL_IMAGE) pushed successfully"

.PHONY: build-push-image ## Build and push container image
build-push-image: build-image push-image
