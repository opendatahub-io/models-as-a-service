package tenantreconcile

import (
	"strings"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// UsageLogsProxyAnnotation selects the installation-wide usage-logs proxy.
const UsageLogsProxyAnnotation = "experimental.maas.opendatahub.io/usage-logs-proxy"

// UsageLogsProxyMode is a supported usage-logs proxy selector.
type UsageLogsProxyMode string

const (
	UsageLogsProxyLegacy  UsageLogsProxyMode = "legacy"
	UsageLogsProxyMaaSAPI UsageLogsProxyMode = "maas-api"
)

func ParseUsageLogsProxyMode(cfg *maasv1alpha1.Config) (UsageLogsProxyMode, bool) {
	if cfg == nil {
		return UsageLogsProxyLegacy, true
	}

	value, found := cfg.Annotations[UsageLogsProxyAnnotation]
	if !found {
		return UsageLogsProxyLegacy, true
	}

	switch UsageLogsProxyMode(strings.TrimSpace(value)) {
	case UsageLogsProxyLegacy:
		return UsageLogsProxyLegacy, true
	case UsageLogsProxyMaaSAPI:
		return UsageLogsProxyMaaSAPI, true
	default:
		return UsageLogsProxyLegacy, false
	}
}
