PROJECT_DIR := $(shell dirname $(abspath $(lastword $(MAKEFILE_LIST))))
LOCALBIN := $(PROJECT_DIR)/bin/tools

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tools
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint
OPERATOR_CHAOS ?= $(LOCALBIN)/operator-chaos

GOLANGCI_LINT_VERSION ?= v2.6.2
OPERATOR_CHAOS_VERSION ?= ec5cd239b770735cab3028d3594385950b51e327
# Target the versioned binary so version bumps trigger reinstall
$(GOLANGCI_LINT)-$(GOLANGCI_LINT_VERSION): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
$(GOLANGCI_LINT): $(GOLANGCI_LINT)-$(GOLANGCI_LINT_VERSION)

$(OPERATOR_CHAOS)-$(OPERATOR_CHAOS_VERSION): $(LOCALBIN)
	$(call go-install-tool,$(OPERATOR_CHAOS),github.com/opendatahub-io/operator-chaos/cmd/operator-chaos,$(OPERATOR_CHAOS_VERSION))
$(OPERATOR_CHAOS): $(OPERATOR_CHAOS)-$(OPERATOR_CHAOS_VERSION)

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary (ideally with version)
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
