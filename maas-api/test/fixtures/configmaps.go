package fixtures

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
)

// Standard tier configuration used across tests.
const TierConfigYAML = `
- name: free
  displayName: Free Tier
  description: Free tier
  level: 1
  groups:
  - system:authenticated
  - tier-free-users
- name: premium
  displayName: Premium Tier
  description: Premium tier
  level: 10
  groups:
  - tier-premium-users
  - beta-testers
- name: developer
  displayName: Developer Tier
  description: Developer tier
  level: 15
  groups:
  - developer-users
- name: enterprise
  displayName: Enterprise Tier
  description: Enterprise tier
  level: 20
  groups:
  - tier-enterprise-users
  - admin-users
`

// CreateTierConfigMap creates a ConfigMap with tier configuration.
func CreateTierConfigMap(namespace string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      constant.TierMappingConfigMap,
			Namespace: namespace,
		},
		Data: map[string]string{
			"tiers": TierConfigYAML,
		},
	}
}
