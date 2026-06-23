package main

import (
	"os"
	"strings"
)

const applicationsNamespaceEnv = "APPLICATIONS_NAMESPACE"

// resolveApplicationsNamespace returns the namespace where maas-controller runs
// and where maas-api workloads are deployed.
//
// Multi-tenant note: In the current architecture, all Tenant CRs share this same
// maas-api deployment namespace. Multi-tenancy is implemented at the logical level
// via separate database tables, AuthPolicies, and Subscriptions, not via separate
// namespaces per tenant.
//
// Fallback priority:
//  1. --maas-api-namespace flag (explicit override for local development)
//  2. APPLICATIONS_NAMESPACE env (injected by ODH module framework via injectModuleEnv)
//  3. MAAS_API_NAMESPACE env (downward API, used in component handler deployments)
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
