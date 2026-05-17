package authpolicy

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

// Lister lists MaaSAuthPolicy CRs from the informer cache.
type Lister interface {
	List() ([]*unstructured.Unstructured, error)
}

// Checker determines whether a user has access to a specific model
// by evaluating MaaSAuthPolicy CRs from the informer cache.
type Checker struct {
	lister Lister
	logger *logger.Logger
}

func NewChecker(log *logger.Logger, lister Lister) *Checker {
	if log == nil {
		log = logger.Production()
	}
	return &Checker{lister: lister, logger: log}
}

// IsModelAccessible returns true if any MaaSAuthPolicy grants the user or
// any of their groups access to the specified model (name + namespace).
func (c *Checker) IsModelAccessible(groups []string, username string, modelName, modelNamespace string) bool {
	policies, err := c.lister.List()
	if err != nil {
		c.logger.Error("Failed to list MaaSAuthPolicy CRs for model access check", "error", err)
		return false
	}

	for _, policy := range policies {
		if c.policyGrantsAccess(policy, groups, username, modelName, modelNamespace) {
			return true
		}
	}
	return false
}

func (c *Checker) policyGrantsAccess(policy *unstructured.Unstructured, groups []string, username, modelName, modelNamespace string) bool {
	spec, ok := policy.Object["spec"].(map[string]any)
	if !ok {
		return false
	}

	if !policyReferencesModel(spec, modelName, modelNamespace) {
		return false
	}

	return policyMatchesSubject(spec, groups, username)
}

func policyReferencesModel(spec map[string]any, modelName, modelNamespace string) bool {
	modelRefs, ok := spec["modelRefs"].([]any)
	if !ok {
		return false
	}
	for _, ref := range modelRefs {
		refMap, ok := ref.(map[string]any)
		if !ok {
			continue
		}
		name, _ := refMap["name"].(string)
		ns, _ := refMap["namespace"].(string)
		if name == modelName && ns == modelNamespace {
			return true
		}
	}
	return false
}

func policyMatchesSubject(spec map[string]any, groups []string, username string) bool {
	subjects, ok := spec["subjects"].(map[string]any)
	if !ok {
		return false
	}

	if username != "" {
		if users, ok := subjects["users"].([]any); ok {
			for _, u := range users {
				if s, ok := u.(string); ok && strings.TrimSpace(s) == username {
					return true
				}
			}
		}
	}

	if groupRefs, ok := subjects["groups"].([]any); ok {
		groupSet := make(map[string]bool, len(groups))
		for _, g := range groups {
			groupSet[strings.TrimSpace(g)] = true
		}
		for _, g := range groupRefs {
			gMap, ok := g.(map[string]any)
			if !ok {
				continue
			}
			name, _ := gMap["name"].(string)
			if groupSet[strings.TrimSpace(name)] {
				return true
			}
		}
	}

	return false
}
