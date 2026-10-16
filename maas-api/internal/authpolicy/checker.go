package authpolicy

import (
	"errors"
	"fmt"
	"slices"
	"sort"
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

// ModelAccessSubjects is the organization-scoped subject inventory for a model.
type ModelAccessSubjects struct {
	Groups []GroupReference `json:"groups,omitempty"`
	Users  []string         `json:"users,omitempty"`
}

// GroupReference mirrors the MaaSAuthPolicy and MaaSSubscription subject
// group shape so the inventory response can carry additional fields later.
type GroupReference struct {
	Name string `json:"name"`
}

// SubjectInventoryProvider builds admin-only subject inventories from the
// tenant-scoped MaaSAuthPolicy cache.
type SubjectInventoryProvider interface {
	ModelAccessSubjects(modelRefs []ModelKey, organizationID string) (map[ModelKey]ModelAccessSubjects, error)
}

// AuthorizedModels returns the set of models the user is authorized to access.
// An empty set means the policy evaluation succeeded but granted no models.
// An error means the policy evaluation could not be completed.
func (c *Checker) AuthorizedModels(groups []string, username string) (map[ModelKey]bool, error) {
	if c.lister == nil {
		c.logger.Error("MaaSAuthPolicy lister is nil; denying model access check")
		return nil, errors.New("MaaSAuthPolicy lister is nil")
	}

	policies, err := c.lister.List()
	if err != nil {
		c.logger.Error("Failed to list MaaSAuthPolicy CRs for model access check", "error", err)
		return nil, fmt.Errorf("failed to list MaaSAuthPolicy CRs: %w", err)
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
	return authorized, nil
}

// IsModelAccessible returns true if any MaaSAuthPolicy grants the user or
// any of their groups access to the specified model (name + namespace).
func (c *Checker) IsModelAccessible(groups []string, username string, modelName, modelNamespace string) bool {
	authorized, err := c.AuthorizedModels(groups, username)
	if err != nil {
		return false
	}
	return authorized[ModelKey{Namespace: modelNamespace, Name: modelName}]
}

// ModelAccessSubjects returns subjects for the exact requested model refs. Groups
// and users are organization-qualified by the exact organization UUID prefix.
func (c *Checker) ModelAccessSubjects(modelRefs []ModelKey, organizationID string) (map[ModelKey]ModelAccessSubjects, error) {
	if c.lister == nil {
		return nil, errors.New("MaaSAuthPolicy lister is nil")
	}
	if organizationID == "" {
		return nil, errors.New("organization id is required")
	}

	requested := make(map[ModelKey]struct{}, len(modelRefs))
	for _, ref := range modelRefs {
		if ref.Namespace == "" || ref.Name == "" {
			return nil, errors.New("model ref namespace and name are required")
		}
		requested[ref] = struct{}{}
	}
	inventory := make(map[ModelKey]ModelAccessSubjects, len(requested))
	if len(requested) == 0 {
		return inventory, nil
	}

	policies, err := c.lister.List()
	if err != nil {
		return nil, fmt.Errorf("failed to list MaaSAuthPolicy resources: %w", err)
	}
	organizationPrefix := organizationID + "/"

	for _, policy := range policies {
		spec, ok := policy.Object["spec"].(map[string]any)
		if !ok {
			continue
		}
		policyGroups := parsePolicyGroups(spec, organizationPrefix)
		policyUsers := parsePolicyUsers(spec, organizationPrefix)

		for _, rawRef := range parseModelRefs(spec) {
			ref := ModelKey{Namespace: rawRef.namespace, Name: rawRef.name}
			if _, exact := requested[ref]; !exact {
				continue
			}

			subjects := inventory[ref]
			subjects.Groups = appendUniqueGroups(subjects.Groups, policyGroups)
			subjects.Users = appendUniqueStrings(subjects.Users, policyUsers)
			inventory[ref] = subjects
		}
	}

	for ref, subjects := range inventory {
		sort.Slice(subjects.Groups, func(i, j int) bool {
			return subjects.Groups[i].Name < subjects.Groups[j].Name
		})
		sort.Strings(subjects.Users)
		inventory[ref] = subjects
	}
	return inventory, nil
}

type parsedModelRef struct {
	namespace string
	name      string
}

func parseModelRefs(spec map[string]any) []parsedModelRef {
	rawRefs, ok := spec["modelRefs"].([]any)
	if !ok {
		return nil
	}
	refs := make([]parsedModelRef, 0, len(rawRefs))
	for _, rawRef := range rawRefs {
		refMap, ok := rawRef.(map[string]any)
		if !ok {
			continue
		}
		name, _ := refMap["name"].(string)
		namespace, _ := refMap["namespace"].(string)
		if name != "" && namespace != "" {
			refs = append(refs, parsedModelRef{namespace: namespace, name: name})
		}
	}
	return refs
}

func parsePolicyGroups(spec map[string]any, organizationPrefix string) []GroupReference {
	subjects, ok := spec["subjects"].(map[string]any)
	if !ok {
		return nil
	}
	rawGroups, ok := subjects["groups"].([]any)
	if !ok {
		return nil
	}

	var organizationGroups []GroupReference
	for _, rawGroup := range rawGroups {
		groupMap, ok := rawGroup.(map[string]any)
		if !ok {
			continue
		}
		name, _ := groupMap["name"].(string)
		if name == "" {
			continue
		}
		if strings.HasPrefix(name, organizationPrefix) {
			organizationGroups = append(organizationGroups, GroupReference{Name: name})
		}
	}
	return organizationGroups
}

func parsePolicyUsers(spec map[string]any, organizationPrefix string) []string {
	subjects, ok := spec["subjects"].(map[string]any)
	if !ok {
		return nil
	}
	rawUsers, ok := subjects["users"].([]any)
	if !ok {
		return nil
	}
	users := make([]string, 0, len(rawUsers))
	for _, rawUser := range rawUsers {
		user, ok := rawUser.(string)
		if ok && strings.HasPrefix(user, organizationPrefix) {
			users = append(users, user)
		}
	}
	return users
}

func appendUniqueGroups(values []GroupReference, additions []GroupReference) []GroupReference {
	for _, addition := range additions {
		if !slices.ContainsFunc(values, func(value GroupReference) bool {
			return value.Name == addition.Name
		}) {
			values = append(values, addition)
		}
	}
	return values
}

func appendUniqueStrings(values []string, additions []string) []string {
	for _, addition := range additions {
		if !slices.Contains(values, addition) {
			values = append(values, addition)
		}
	}
	return values
}

func policyMatchesSubject(spec map[string]any, groups []string, username string) bool {
	subjects, ok := spec["subjects"].(map[string]any)
	if !ok {
		return false
	}

	if username != "" {
		if users, ok := subjects["users"].([]any); ok {
			for _, u := range users {
				if s, ok := u.(string); ok && strings.TrimSpace(s) == strings.TrimSpace(username) {
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
