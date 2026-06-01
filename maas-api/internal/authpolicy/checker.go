package authpolicy

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

var authPolicyGVR = schema.GroupVersionResource{
	Group:    "maas.opendatahub.io",
	Version:  "v1alpha1",
	Resource: "maasauthpolicies",
}

// GVR returns the GroupVersionResource for MaaSAuthPolicy CRs.
func GVR() schema.GroupVersionResource {
	return authPolicyGVR
}

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
	if lister == nil {
		log.Error("MaaSAuthPolicy lister is nil; all model access checks will be denied")
	}
	return &Checker{lister: lister, logger: log}
}

// ModelKey uniquely identifies a model by namespace and name.
type ModelKey struct {
	Namespace string
	Name      string
}

// AuthorizedModels returns the set of models the user is authorized to access.
// Callers should build this set once per request and test membership via map lookup.
func (c *Checker) AuthorizedModels(groups []string, username string) map[ModelKey]bool {
	if c.lister == nil {
		c.logger.Error("MaaSAuthPolicy lister is nil; denying model access check")
		return nil
	}

	policies, err := c.lister.List()
	if err != nil {
		c.logger.Error("Failed to list MaaSAuthPolicy CRs for model access check", "error", err)
		return nil
	}

	authorized := make(map[ModelKey]bool)
	for _, policy := range policies {
		spec, ok := policy.Object["spec"].(map[string]any)
		if !ok {
			continue
		}
		if !policyMatchesSubject(spec, groups, username) {
			continue
		}
		modelRefs, ok := spec["modelRefs"].([]any)
		if !ok {
			continue
		}
		for _, ref := range modelRefs {
			refMap, ok := ref.(map[string]any)
			if !ok {
				continue
			}
			name, _ := refMap["name"].(string)
			ns, _ := refMap["namespace"].(string)
			if name != "" {
				authorized[ModelKey{Namespace: ns, Name: name}] = true
			}
		}
	}
	return authorized
}

// IsModelAccessible returns true if any MaaSAuthPolicy grants the user or
// any of their groups access to the specified model (name + namespace).
func (c *Checker) IsModelAccessible(groups []string, username string, modelName, modelNamespace string) bool {
	authorized := c.AuthorizedModels(groups, username)
	if authorized == nil {
		return false
	}
	return authorized[ModelKey{Namespace: modelNamespace, Name: modelName}]
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
