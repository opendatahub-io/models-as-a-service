package main

import (
	"os"
	"strings"
)

const applicationsNamespaceEnv = "APPLICATIONS_NAMESPACE"

// resolveApplicationsNamespace returns the namespace where maas-controller and
// co-located platform workloads (maas-api, etc.) run. ODH module installs set
// APPLICATIONS_NAMESPACE via injectModuleEnv; --maas-api-namespace overrides for
// local development. MAAS_API_NAMESPACE (downward API) is a fallback when the
// controller pod runs in the applications namespace.
func resolveApplicationsNamespace(flagValue string) string {
	if v := strings.TrimSpace(flagValue); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(applicationsNamespaceEnv)); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("MAAS_API_NAMESPACE")); v != "" {
		return v
	}
	return ""
}
