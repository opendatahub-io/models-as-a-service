package tenantreconcile

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func TestParseUsageLogsProxyMode(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *maasv1alpha1.Config
		wantMode  UsageLogsProxyMode
		wantValid bool
	}{
		{name: "nil Config defaults to legacy", wantMode: UsageLogsProxyLegacy, wantValid: true},
		{name: "unset annotation defaults to legacy", cfg: &maasv1alpha1.Config{}, wantMode: UsageLogsProxyLegacy, wantValid: true},
		{
			name:      "legacy",
			cfg:       usageLogsProxyConfigFor("legacy"),
			wantMode:  UsageLogsProxyLegacy,
			wantValid: true,
		},
		{
			name:      "maas-api",
			cfg:       usageLogsProxyConfigFor("maas-api"),
			wantMode:  UsageLogsProxyMaaSAPI,
			wantValid: true,
		},
		{
			name:      "invalid value fails closed to legacy",
			cfg:       usageLogsProxyConfigFor("python"),
			wantMode:  UsageLogsProxyLegacy,
			wantValid: false,
		},
		{
			name:      "empty value is invalid",
			cfg:       usageLogsProxyConfigFor(""),
			wantMode:  UsageLogsProxyLegacy,
			wantValid: false,
		},
		{
			name:      "surrounding whitespace is trimmed",
			cfg:       usageLogsProxyConfigFor(" maas-api "),
			wantMode:  UsageLogsProxyMaaSAPI,
			wantValid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, valid := ParseUsageLogsProxyMode(tt.cfg)
			require.Equal(t, tt.wantMode, mode)
			require.Equal(t, tt.wantValid, valid)
		})
	}
}

func usageLogsProxyConfigFor(value string) *maasv1alpha1.Config {
	return &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{UsageLogsProxyAnnotation: value},
		},
	}
}
