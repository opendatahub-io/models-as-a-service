package maas

// OptionalAPIGroups lists API groups whose CRDs are installed by optional platform
// components (e.g. COO for Perses). Resources in these groups are skipped gracefully
// when their CRDs are not yet registered, instead of failing the Tenant reconcile.
// The CRD watch in the controller re-triggers reconcile once the CRDs appear.
var OptionalAPIGroups = map[string]bool{
	"perses.dev":             true, // Cluster Observability Operator (COO) — Perses dashboards and datasources
	"kuadrant.io":            true, // Kuadrant — rate-limiting policies (optional, not required for core MaaS)
	"extensions.kuadrant.io": true, // Kuadrant extensions — TelemetryPolicy (optional)
}

// isOptionalAPIGroup returns true when missing CRDs for the given group should not
// fail the reconcile (i.e. the dependency is installed by an optional operator).
func isOptionalAPIGroup(group string) bool {
	return OptionalAPIGroups[group]
}
