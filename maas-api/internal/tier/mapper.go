package tier

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	"gopkg.in/yaml.v3"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	corelisters "k8s.io/client-go/listers/core/v1"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
)

// Mapper handles tier-to-group mapping lookups.
type Mapper struct {
	tenantName      string
	namespace       string
	configMapLister corelisters.ConfigMapLister
	logger          logr.Logger
}

func NewMapper(log logr.Logger, configMapLister corelisters.ConfigMapLister, tenantName, namespace string) *Mapper {
	return &Mapper{
		tenantName:      tenantName,
		namespace:       namespace,
		configMapLister: configMapLister,
		logger:          log,
	}
}

func (m *Mapper) Namespace(tier string) (string, error) {
	tiers, err := m.loadTierConfig()
	if err != nil {
		return "", err
	}

	for i := range tiers {
		if tiers[i].Name == tier {
			return m.ProjectedNsName(&tiers[i]), nil
		}
	}

	return "", fmt.Errorf("tier %s not found", tier)
}

// GetTierForGroups returns the highest level tier for a user with multiple group memberships.
//
// Returns error if no groups provided or no groups found in any tier.
func (m *Mapper) GetTierForGroups(groups ...string) (*Tier, error) {
	if len(groups) == 0 {
		return nil, errors.New("no groups provided")
	}

	tiers, err := m.loadTierConfig()
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, fmt.Errorf("tier mapping not found, provide configuration in %s", constant.TierMappingConfigMap)
		}
		m.logger.Error(err, "Failed to load tier configuration from ConfigMap",
			"configmap", constant.TierMappingConfigMap,
		)
		return nil, fmt.Errorf("failed to load tier configuration: %w", err)
	}

	sort.SliceStable(tiers, func(i, j int) bool {
		return tiers[i].Level > tiers[j].Level
	})

	for i := range tiers {
		for _, userGroup := range groups {
			if slices.Contains(tiers[i].Groups, userGroup) {
				return &tiers[i], nil
			}
		}
	}

	return nil, &GroupNotFoundError{Group: fmt.Sprintf("groups [%s]", strings.Join(groups, ", "))}
}

// ProjectedSAGroup returns the projected SA group for a tier.
func (m *Mapper) ProjectedSAGroup(tier *Tier) string {
	return fmt.Sprintf("system:serviceaccounts:%s", m.ProjectedNsName(tier))
}

func (m *Mapper) ProjectedNsName(tier *Tier) string {
	return fmt.Sprintf("%s-tier-%s", m.tenantName, tier.Name)
}

func (m *Mapper) loadTierConfig() ([]Tier, error) {
	cm, err := m.configMapLister.ConfigMaps(m.namespace).Get(constant.TierMappingConfigMap)
	if err != nil {
		return nil, err
	}

	configData, exists := cm.Data["tiers"]
	if !exists {
		m.logger.V(1).Info("Tiers key not found in ConfigMap",
			"configmap", constant.TierMappingConfigMap,
		)
		return nil, errors.New("tier to group mapping configuration not found")
	}

	var tiers []Tier
	if err := yaml.Unmarshal([]byte(configData), &tiers); err != nil {
		return nil, fmt.Errorf("failed to parse tier configuration: %w", err)
	}

	// Validate tier configuration on every load
	if err := validateTierConfig(tiers); err != nil {
		return nil, fmt.Errorf("invalid tier configuration: %w", err)
	}

	for i := range tiers {
		tier := &tiers[i]
		tier.Groups = append(tier.Groups, m.ProjectedSAGroup(tier))
	}

	return tiers, nil
}

// validateTierConfig validates that tier configuration is valid:
// - All tier names must be unique
// - If displayName is provided, it must be non-empty.
func validateTierConfig(tiers []Tier) error {
	seenNames := make(map[string]bool)

	for i, tier := range tiers {
		if tier.Name == "" {
			return fmt.Errorf("tier at index %d has empty name", i)
		}

		if seenNames[tier.Name] {
			return fmt.Errorf("duplicate tier name %q found", tier.Name)
		}
		seenNames[tier.Name] = true

		if tier.DisplayName != "" && strings.TrimSpace(tier.DisplayName) == "" {
			return fmt.Errorf("tier %q has whitespace-only displayName", tier.Name)
		}
	}

	return nil
}
