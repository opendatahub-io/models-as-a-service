package maas

const (
	// DefaultMaaSDiscoveryImage is the default image for the maas-discovery container.
	// Can be overridden via RELATED_IMAGE_ODH_MAAS_DISCOVERY_IMAGE for disconnected environments.
	DefaultMaaSDiscoveryImage = "quay.io/opendatahub/odh-maas-discovery@sha256:678af94dfbdbb92c0f4f29ea5719603d55eddc485136ad723f85d2261d0075ec"

	// DefaultAITenantBootstrappedAnnotation records that the default AITenant was
	// bootstrapped successfully and must not be recreated after an intentional deletion.
	DefaultAITenantBootstrappedAnnotation = "maas.opendatahub.io/default-aitenant-bootstrapped"
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
