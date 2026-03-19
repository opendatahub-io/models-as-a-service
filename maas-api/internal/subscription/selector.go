package subscription

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

// Lister provides access to MaaSSubscription resources from an informer cache.
type Lister interface {
	List() ([]*unstructured.Unstructured, error)
}

// Selector handles subscription selection logic.
type Selector struct {
	lister Lister
	logger *logger.Logger
}

// NewSelector creates a new subscription selector.
func NewSelector(log *logger.Logger, lister Lister) *Selector {
	if log == nil {
		log = logger.Production()
	}
	return &Selector{
		lister: lister,
		logger: log,
	}
}

// subscription represents a parsed MaaSSubscription for selection.
type subscription struct {
	Name           string
	Namespace      string
	DisplayName    string
	Description    string
	Groups         []string
	Users          []string
	Priority       int32
	MaxLimit       int64
	OrganizationID string
	CostCenter     string
	Labels         map[string]string
	ModelRefs      []modelRef
}

// modelRef represents a reference to a model in a subscription.
type modelRef struct {
	Namespace string
	Name      string
}

// GetAllAccessible returns all subscriptions the user has access to.
func (s *Selector) GetAllAccessible(groups []string, username string) ([]*SelectResponse, error) {
	if len(groups) == 0 && username == "" {
		return nil, errors.New("either groups or username must be provided")
	}

	subscriptions, err := s.loadSubscriptions()
	if err != nil {
		return nil, fmt.Errorf("failed to load subscriptions: %w", err)
	}

	var accessible []*SelectResponse
	for _, sub := range subscriptions {
		if userHasAccess(&sub, username, groups) {
			accessible = append(accessible, toResponse(&sub))
		}
	}

	// Sort for deterministic ordering
	sort.Slice(accessible, func(i, j int) bool {
		return accessible[i].Name < accessible[j].Name
	})

	return accessible, nil
}

// Select implements the subscription selection logic.
// Returns the selected subscription or an error if none found.
// If requestedModel is provided, validates that the selected subscription includes that model.
func (s *Selector) Select(groups []string, username string, requestedSubscription string, requestedModel string) (*SelectResponse, error) {
	if len(groups) == 0 && username == "" {
		return nil, errors.New("either groups or username must be provided")
	}

	subscriptions, err := s.loadSubscriptions()
	if err != nil {
		return nil, fmt.Errorf("failed to load subscriptions: %w", err)
	}

	if len(subscriptions) == 0 {
		return nil, &NoSubscriptionError{}
	}

	// Sort by priority (desc), then maxLimit (desc)
	sortSubscriptionsByPriority(subscriptions)

	// Branch 1: Explicit subscription selection (with validation)
	// Support both formats: "namespace/name" and bare "name"
	if requestedSubscription != "" {
		// First, try exact qualified match (namespace/name)
		for _, sub := range subscriptions {
			qualifiedName := fmt.Sprintf("%s/%s", sub.Namespace, sub.Name)
			if qualifiedName == requestedSubscription {
				if !userHasAccess(&sub, username, groups) {
					return nil, &AccessDeniedError{Subscription: requestedSubscription}
				}
				// Validate subscription includes the requested model
				if requestedModel != "" && !subscriptionIncludesModel(&sub, requestedModel) {
					return nil, &ModelNotInSubscriptionError{Subscription: requestedSubscription, Model: requestedModel}
				}
				return toResponse(&sub), nil
			}
		}

		// If no qualified match found and request is bare name (no '/'), try bare name matching
		if !strings.Contains(requestedSubscription, "/") {
			var matches []subscription
			for _, sub := range subscriptions {
				if sub.Name == requestedSubscription {
					matches = append(matches, sub)
				}
			}

			// Filter matches by access before exposing namespace information
			var accessibleMatches []subscription
			for _, sub := range matches {
				if userHasAccess(&sub, username, groups) {
					accessibleMatches = append(accessibleMatches, sub)
				}
			}

			if len(accessibleMatches) == 0 {
				// No accessible matches - don't expose namespaces of inaccessible subscriptions
				return nil, &SubscriptionNotFoundError{Subscription: requestedSubscription}
			}

			if len(accessibleMatches) > 1 {
				// Multiple accessible subscriptions with same bare name in different namespaces
				namespaces := make([]string, len(accessibleMatches))
				for i, m := range accessibleMatches {
					namespaces[i] = m.Namespace
				}
				return nil, &SubscriptionAmbiguousError{
					Subscription: requestedSubscription,
					Namespaces:   namespaces,
				}
			}

			// Exactly one accessible match - use it
			if requestedModel != "" && !subscriptionIncludesModel(&accessibleMatches[0], requestedModel) {
				return nil, &ModelNotInSubscriptionError{Subscription: requestedSubscription, Model: requestedModel}
			}
			return toResponse(&accessibleMatches[0]), nil
		}

		// Request had '/' but no match found
		return nil, &SubscriptionNotFoundError{Subscription: requestedSubscription}
	}

	// Branch 2: Auto-selection
	var accessibleSubs []subscription
	for _, sub := range subscriptions {
		if userHasAccess(&sub, username, groups) {
			// If model is specified, only include subscriptions that contain that model
			if requestedModel != "" && !subscriptionIncludesModel(&sub, requestedModel) {
				continue
			}
			accessibleSubs = append(accessibleSubs, sub)
		}
	}

	if len(accessibleSubs) == 0 {
		return nil, &NoSubscriptionError{}
	}

	if len(accessibleSubs) == 1 {
		return toResponse(&accessibleSubs[0]), nil
	}

	// User has multiple subscriptions - require explicit selection
	subNames := make([]string, len(accessibleSubs))
	for i, sub := range accessibleSubs {
		subNames[i] = sub.Name
	}
	return nil, &MultipleSubscriptionsError{Subscriptions: subNames}
}

// loadSubscriptions fetches and parses MaaSSubscription resources.
func (s *Selector) loadSubscriptions() ([]subscription, error) {
	objects, err := s.lister.List()
	if err != nil {
		return nil, err
	}

	subscriptions := make([]subscription, 0, len(objects))
	for _, obj := range objects {
		sub, err := parseSubscription(obj)
		if err != nil {
			s.logger.Warn("Failed to parse subscription, skipping",
				"name", obj.GetName(),
				"namespace", obj.GetNamespace(),
				"error", err,
			)
			continue
		}
		subscriptions = append(subscriptions, sub)
	}

	return subscriptions, nil
}

// parseSubscription extracts subscription data from unstructured object.
func parseSubscription(obj *unstructured.Unstructured) (subscription, error) {
	spec, found, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil || !found {
		return subscription{}, errors.New("spec not found")
	}

	sub := subscription{
		Name:      obj.GetName(),
		Namespace: obj.GetNamespace(),
	}

	// Parse displayName (optional - field may not exist in CRD yet)
	if displayName, found, _ := unstructured.NestedString(spec, "displayName"); found {
		sub.DisplayName = displayName
	}

	// Parse description (optional - field may not exist in CRD yet)
	if description, found, _ := unstructured.NestedString(spec, "description"); found {
		sub.Description = description
	}

	// Parse owner
	if owner, found, _ := unstructured.NestedMap(spec, "owner"); found {
		// Parse groups
		if groupsRaw, found, _ := unstructured.NestedSlice(owner, "groups"); found {
			for _, g := range groupsRaw {
				if groupMap, ok := g.(map[string]any); ok {
					if name, ok := groupMap["name"].(string); ok {
						sub.Groups = append(sub.Groups, name)
					}
				}
			}
		}

		// Parse users
		if users, found, _ := unstructured.NestedStringSlice(owner, "users"); found {
			sub.Users = users
		}
	}

	// Parse priority
	if priority, found, _ := unstructured.NestedInt64(spec, "priority"); found {
		if priority >= 0 && priority <= 2147483647 {
			sub.Priority = int32(priority)
		}
	}

	// Parse modelRefs to calculate maxLimit and extract model references
	if modelRefsRaw, found, _ := unstructured.NestedSlice(spec, "modelRefs"); found {
		for _, refRaw := range modelRefsRaw {
			if modelMap, ok := refRaw.(map[string]any); ok {
				// Extract namespace and name for model validation
				ns, _ := modelMap["namespace"].(string)
				name, _ := modelMap["name"].(string)
				if ns != "" && name != "" {
					sub.ModelRefs = append(sub.ModelRefs, modelRef{Namespace: ns, Name: name})
				}

				// Calculate maxLimit
				if limits, found, _ := unstructured.NestedSlice(modelMap, "tokenRateLimits"); found {
					for _, limitRaw := range limits {
						if limitMap, ok := limitRaw.(map[string]any); ok {
							if limit, ok := limitMap["limit"].(int64); ok {
								if limit > sub.MaxLimit {
									sub.MaxLimit = limit
								}
							}
						}
					}
				}
			}
		}
	}

	// Parse tokenMetadata
	if metadata, found, _ := unstructured.NestedMap(spec, "tokenMetadata"); found {
		if orgID, ok := metadata["organizationId"].(string); ok {
			sub.OrganizationID = orgID
		}
		if costCenter, ok := metadata["costCenter"].(string); ok {
			sub.CostCenter = costCenter
		}
		if labelsRaw, ok := metadata["labels"].(map[string]any); ok {
			sub.Labels = make(map[string]string)
			for k, v := range labelsRaw {
				if s, ok := v.(string); ok {
					sub.Labels[k] = s
				}
			}
		}
	}

	return sub, nil
}

// userHasAccess checks if user/groups match subscription owner.
func userHasAccess(sub *subscription, username string, groups []string) bool {
	// Check username match
	if slices.Contains(sub.Users, username) {
		return true
	}

	// Check group match
	for _, subGroup := range sub.Groups {
		for _, userGroup := range groups {
			userGroup = strings.TrimSpace(userGroup)
			if userGroup == subGroup {
				return true
			}
		}
	}

	return false
}

// subscriptionIncludesModel checks if the subscription's modelRefs includes the requested model.
// requestedModel format: "namespace/name".
func subscriptionIncludesModel(sub *subscription, requestedModel string) bool {
	if requestedModel == "" {
		return true // no model specified, so subscription is valid
	}

	// Parse the requested model (format: "namespace/name")
	parts := strings.SplitN(requestedModel, "/", 2)
	if len(parts) != 2 {
		return false // invalid format
	}
	requestedNS := parts[0]
	requestedName := parts[1]

	// Check if any modelRef in the subscription matches
	for _, ref := range sub.ModelRefs {
		if ref.Namespace == requestedNS && ref.Name == requestedName {
			return true
		}
	}

	return false
}

// sortSubscriptionsByPriority sorts in-place by priority desc, then maxLimit desc.
func sortSubscriptionsByPriority(subs []subscription) {
	sort.SliceStable(subs, func(i, j int) bool {
		if subs[i].Priority != subs[j].Priority {
			return subs[i].Priority > subs[j].Priority
		}
		return subs[i].MaxLimit > subs[j].MaxLimit
	})
}

// toResponse converts internal subscription to API response.
func toResponse(sub *subscription) *SelectResponse {
	// Convert internal modelRef to public ModelRef
	modelRefs := make([]ModelRef, len(sub.ModelRefs))
	for i, ref := range sub.ModelRefs {
		modelRefs[i] = ModelRef(ref)
	}

	return &SelectResponse{
		Name:           sub.Name,
		Namespace:      sub.Namespace,
		DisplayName:    sub.DisplayName,
		Description:    sub.Description,
		ModelRefs:      modelRefs,
		OrganizationID: sub.OrganizationID,
		CostCenter:     sub.CostCenter,
		Labels:         sub.Labels,
	}
}

// NoSubscriptionError indicates no matching subscription found.
type NoSubscriptionError struct{}

func (e *NoSubscriptionError) Error() string {
	return "no matching subscription found for user"
}

// SubscriptionNotFoundError indicates requested subscription doesn't exist.
type SubscriptionNotFoundError struct {
	Subscription string
}

func (e *SubscriptionNotFoundError) Error() string {
	return "requested subscription not found"
}

// AccessDeniedError indicates user doesn't have access to requested subscription.
type AccessDeniedError struct {
	Subscription string
}

func (e *AccessDeniedError) Error() string {
	return "access denied to requested subscription"
}

// MultipleSubscriptionsError indicates user has access to multiple subscriptions and must explicitly select one.
type MultipleSubscriptionsError struct {
	Subscriptions []string
}

func (e *MultipleSubscriptionsError) Error() string {
	return "user has access to multiple subscriptions, must specify subscription using X-MaaS-Subscription header"
}

// SubscriptionAmbiguousError indicates multiple subscriptions with the same bare name exist.
type SubscriptionAmbiguousError struct {
	Subscription string
	Namespaces   []string
}

func (e *SubscriptionAmbiguousError) Error() string {
	return fmt.Sprintf("subscription name '%s' is ambiguous (exists in multiple namespaces: %s), use qualified name 'namespace/name'",
		e.Subscription, strings.Join(e.Namespaces, ", "))
}

// ModelNotInSubscriptionError indicates the requested model is not included in the subscription.
type ModelNotInSubscriptionError struct {
	Subscription string
	Model        string
}

func (e *ModelNotInSubscriptionError) Error() string {
	return fmt.Sprintf("subscription %s does not include model %s", e.Subscription, e.Model)
}
