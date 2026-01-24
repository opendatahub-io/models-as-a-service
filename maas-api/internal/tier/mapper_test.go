package tier_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/tier"
	"github.com/opendatahub-io/models-as-a-service/maas-api/test/fixtures"
)

const (
	testNamespace = fixtures.TestNamespace
	testTenant    = fixtures.TestTenant
)

func TestMapper_GetTierForGroups(t *testing.T) {
	testLogger := logger.Development()
	configMap := fixtures.CreateTierConfigMap(testNamespace)
	mapper := tier.NewMapper(testLogger, fixtures.NewConfigMapLister(configMap), testTenant, testNamespace)

	tests := []struct {
		name          string
		groups        []string
		expectedTier  string
		expectedError bool
		description   string
	}{
		{
			name:         "single group - free tier",
			groups:       []string{"system:authenticated"},
			expectedTier: "free",
			description:  "User belongs to only free tier group",
		},
		{
			name:         "inferred SA group - free tier",
			groups:       []string{"system:serviceaccounts:test-tenant-tier-free"},
			expectedTier: "free",
			description:  "User belongs to only free tier group",
		},
		{
			name:         "inferred SA group - premium tier",
			groups:       []string{"system:serviceaccounts:test-tenant-tier-premium"},
			expectedTier: "premium",
			description:  "User belongs to only premium tier group",
		},
		{
			name:         "single group - premium tier",
			groups:       []string{"tier-premium-users"},
			expectedTier: "premium",
			description:  "User belongs to only premium tier group",
		},
		{
			name:         "single group - enterprise tier",
			groups:       []string{"tier-enterprise-users"},
			expectedTier: "enterprise",
			description:  "User belongs to only enterprise tier group",
		},
		{
			name:         "multiple groups - enterprise wins over free",
			groups:       []string{"system:authenticated", "tier-enterprise-users"},
			expectedTier: "enterprise",
			description:  "User belongs to both free and enterprise - enterprise has higher level (20 > 1)",
		},
		{
			name:         "multiple groups - premium wins over free",
			groups:       []string{"tier-free-users", "tier-premium-users"},
			expectedTier: "premium",
			description:  "User belongs to both free and premium - premium has higher level (10 > 1)",
		},
		{
			name:         "multiple groups - enterprise wins over premium",
			groups:       []string{"tier-premium-users", "tier-enterprise-users"},
			expectedTier: "enterprise",
			description:  "User belongs to both premium and enterprise - enterprise has higher level (20 > 10)",
		},
		{
			name:         "multiple groups - enterprise wins over developer",
			groups:       []string{"developer-users", "tier-enterprise-users"},
			expectedTier: "enterprise",
			description:  "User belongs to both developer and enterprise - enterprise has higher level (20 > 15)",
		},
		{
			name:         "multiple groups - developer wins over premium",
			groups:       []string{"tier-premium-users", "developer-users"},
			expectedTier: "developer",
			description:  "User belongs to both premium and developer - developer has higher level (15 > 10)",
		},
		{
			name:         "multiple groups - service account groups",
			groups:       []string{"system:serviceaccounts", "system:serviceaccounts:test-tenant-tier-premium", "system:authenticated"},
			expectedTier: "premium",
			description:  "User belongs to both premium and developer - developer has higher level (15 > 10)",
		},
		{
			name:         "three groups - enterprise wins",
			groups:       []string{"tier-free-users", "tier-premium-users", "tier-enterprise-users"},
			expectedTier: "enterprise",
			description:  "User belongs to free, premium, and enterprise - enterprise has highest level (20)",
		},
		{
			name:         "all groups - enterprise wins",
			groups:       []string{"system:authenticated", "tier-premium-users", "developer-users", "admin-users"},
			expectedTier: "enterprise",
			description:  "User belongs to groups across all tiers - enterprise has highest level (20)",
		},
		{
			name:          "no groups provided",
			groups:        []string{},
			expectedError: true,
			description:   "Empty groups array should return error",
		},
		{
			name:          "unknown groups",
			groups:        []string{"unknown-group-1", "unknown-group-2"},
			expectedError: true,
			description:   "Groups not found in any tier should return error",
		},
		{
			name:         "mix of known and unknown groups",
			groups:       []string{"tier-premium-users", "unknown-group"},
			expectedTier: "premium",
			description:  "Should find tier for known group and ignore unknown ones",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mappedTier, err := mapper.GetTierForGroups(tt.groups...)

			if tt.expectedError && err == nil {
				t.Errorf("expected error but got none")
				return
			}

			if !tt.expectedError && err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if !tt.expectedError && mappedTier.Name != tt.expectedTier {
				t.Errorf("expected tier name %s, got %s", tt.expectedTier, mappedTier.Name)
			}
		})
	}
}

func TestMapper_GetTierForGroups_SameLevels(t *testing.T) {
	testLogger := logger.Development()
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      constant.TierMappingConfigMap,
			Namespace: testNamespace,
		},
		Data: map[string]string{
			"tiers": `
- name: tier-a
  description: Tier A
  level: 10
  groups:
  - group-a
- name: tier-b
  description: Tier B
  level: 10
  groups:
  - group-b
`,
		},
	}

	mapper := tier.NewMapper(testLogger, fixtures.NewConfigMapLister(configMap), testTenant, testNamespace)

	// When levels are equal, first tier found should win
	mappedTier, err := mapper.GetTierForGroups("group-a", "group-b")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	// Should return tier-a since it appears first in the config and has same level
	if mappedTier.Name != "tier-a" {
		t.Errorf("expected tier name 'tier-a', got %s", mappedTier.Name)
	}
}

func TestMapper_GetTierForGroups_InvalidConfig(t *testing.T) {
	testLogger := logger.Development()
	tests := []struct {
		name        string
		tiersYAML   string
		errContains string
	}{
		{
			name: "duplicate tier names",
			tiersYAML: `
- name: free
  level: 0
  groups:
  - group-a
- name: free
  level: 1
  groups:
  - group-b
`,
			errContains: "duplicate tier name",
		},
		{
			name: "whitespace-only displayName",
			tiersYAML: `
- name: free
  displayName: "   "
  level: 0
  groups:
  - group-a
`,
			errContains: "whitespace-only displayName",
		},
		{
			name: "empty tier name",
			tiersYAML: `
- name: ""
  level: 0
  groups:
  - group-a
`,
			errContains: "empty name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      constant.TierMappingConfigMap,
					Namespace: testNamespace,
				},
				Data: map[string]string{
					"tiers": tt.tiersYAML,
				},
			}

			mapper := tier.NewMapper(testLogger, fixtures.NewConfigMapLister(configMap), testTenant, testNamespace)

			_, err := mapper.GetTierForGroups("group-a")
			if err == nil {
				t.Errorf("expected error containing %q, got nil", tt.errContains)
				return
			}

			if !strings.Contains(err.Error(), tt.errContains) {
				t.Errorf("expected error containing %q, got %v", tt.errContains, err)
			}
		})
	}
}
