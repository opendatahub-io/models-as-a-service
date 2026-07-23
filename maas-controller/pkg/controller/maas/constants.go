package maas

const (
	// DefaultUsageLogsTenancyProxyImage is the default image for the usage-logs tenancy proxy container.
	// Can be overridden via RELATED_IMAGE_ODH_PYTHON_312_IMAGE for disconnected environments.
	DefaultUsageLogsTenancyProxyImage = "registry.redhat.io/ubi9/python-312@sha256:f6713d327d37e654a443752e6654b5aab88f31690e1161eed9c34dd837870172"
)

// OptionalAPIGroups lists API groups whose CRDs are installed by optional platform
// components (e.g. COO for Perses). Resources in these groups are skipped gracefully
// when their CRDs are not yet registered, instead of failing the Tenant reconcile.
// The CRD watch in the controller re-triggers reconcile once the CRDs appear.
var OptionalAPIGroups = map[string]bool{
	"perses.dev":       true, // Cluster Observability Operator (COO) — Perses dashboards and datasources
	"opentelemetry.io": true, // OpenTelemetry Collector
}

// isOptionalAPIGroup returns true when missing CRDs for the given group should not
// fail the reconcile (i.e. the dependency is installed by an optional operator).
func isOptionalAPIGroup(group string) bool {
	return OptionalAPIGroups[group]
}
