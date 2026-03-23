package subscription

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
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
	ModelRefs      []ModelRefInfo
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
				// Multiple accessible subscriptions with same bare name in different namespaces.
				// Don't leak any information - return a generic error requiring qualified names.
				return nil, &SubscriptionAmbiguousError{}
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

	// Parse annotations for display metadata
	if annotations := obj.GetAnnotations(); annotations != nil {
		sub.DisplayName = annotations[constant.AnnotationDisplayName]
		sub.Description = annotations[constant.AnnotationDescription]
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

	// Parse modelRefs
	if modelRefs, found, _ := unstructured.NestedSlice(spec, "modelRefs"); found {
		for _, modelRef := range modelRefs {
			if modelMap, ok := modelRef.(map[string]any); ok {
				ref := parseModelRef(modelMap)
				for _, trl := range ref.TokenRateLimits {
					if trl.Limit > sub.MaxLimit {
						sub.MaxLimit = trl.Limit
					}
				}
				sub.ModelRefs = append(sub.ModelRefs, ref)
			}
		}
	}

	// Parse tokenMetadata
	parseTokenMetadata(spec, &sub)

	return sub, nil
}

// parseModelRef extracts a ModelRefInfo from an unstructured model ref map.
func parseModelRef(modelMap map[string]any) ModelRefInfo {
	ref := ModelRefInfo{}
	if name, ok := modelMap["name"].(string); ok {
		ref.Name = name
	}
	if ns, ok := modelMap["namespace"].(string); ok {
		ref.Namespace = ns
	}
	if limits, found, _ := unstructured.NestedSlice(modelMap, "tokenRateLimits"); found {
		for _, limitRaw := range limits {
			if limitMap, ok := limitRaw.(map[string]any); ok {
				trl := TokenRateLimit{}
				if limit, ok := limitMap["limit"].(int64); ok {
					trl.Limit = limit
				}
				if window, ok := limitMap["window"].(string); ok {
					trl.Window = window
				}
				ref.TokenRateLimits = append(ref.TokenRateLimits, trl)
			}
		}
	}
	if billingRate, found, _ := unstructured.NestedMap(modelMap, "billingRate"); found {
		br := &BillingRate{}
		if perToken, ok := billingRate["perToken"].(string); ok {
			br.PerToken = perToken
		}
		ref.BillingRate = br
	}
	return ref
}

// parseTokenMetadata extracts tokenMetadata fields from the spec into the subscription.
func parseTokenMetadata(spec map[string]any, sub *subscription) {
	metadata, found, _ := unstructured.NestedMap(spec, "tokenMetadata")
	if !found {
		return
	}
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

// hasModel returns true if the subscription includes the given model name.
func (s subscription) hasModel(modelID string) bool {
	for _, ref := range s.ModelRefs {
		if ref.Name == modelID {
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

// ListAccessibleForModel returns subscriptions the user has access to
// that include the specified model in their modelRefs.
func (s *Selector) ListAccessibleForModel(username string, groups []string, modelID string) ([]SubscriptionInfo, error) {
	subscriptions, err := s.loadSubscriptions()
	if err != nil {
		return nil, fmt.Errorf("failed to load subscriptions: %w", err)
	}

	result := []SubscriptionInfo{}
	for _, sub := range subscriptions {
		if userHasAccess(&sub, username, groups) && sub.hasModel(modelID) {
			result = append(result, toSubscriptionInfo(&sub))
		}
	}

	// Sort for deterministic ordering
	sort.Slice(result, func(i, j int) bool {
		return result[i].SubscriptionIDHeader < result[j].SubscriptionIDHeader
	})

	return result, nil
}

// toSubscriptionInfo converts internal subscription to a list response item.
func toSubscriptionInfo(sub *subscription) SubscriptionInfo {
	desc := sub.Description
	if desc == "" {
		desc = sub.DisplayName
	}
	if desc == "" {
		desc = sub.Name
	}
	modelRefs := sub.ModelRefs
	if modelRefs == nil {
		modelRefs = []ModelRefInfo{}
	}
	return SubscriptionInfo{
		SubscriptionIDHeader:    sub.Name,
		SubscriptionDescription: desc,
		DisplayName:             sub.DisplayName,
		Priority:                sub.Priority,
		ModelRefs:               modelRefs,
		OrganizationID:          sub.OrganizationID,
		CostCenter:              sub.CostCenter,
		Labels:                  sub.Labels,
	}
}

// ResponseToSubscriptionInfo converts a SelectResponse to a SubscriptionInfo.
func ResponseToSubscriptionInfo(sub *SelectResponse) SubscriptionInfo {
	desc := sub.Description
	if desc == "" {
		desc = sub.DisplayName
	}
	if desc == "" {
		desc = sub.Name
	}
	modelRefs := sub.ModelRefs
	if modelRefs == nil {
		modelRefs = []ModelRefInfo{}
	}
	return SubscriptionInfo{
		SubscriptionIDHeader:    sub.Name,
		SubscriptionDescription: desc,
		DisplayName:             sub.DisplayName,
		Priority:                sub.Priority,
		ModelRefs:               modelRefs,
		OrganizationID:          sub.OrganizationID,
		CostCenter:              sub.CostCenter,
		Labels:                  sub.Labels,
	}
}

// toResponse converts internal subscription to API response.
func toResponse(sub *subscription) *SelectResponse {
	modelRefs := sub.ModelRefs
	if modelRefs == nil {
		modelRefs = []ModelRefInfo{}
	}
	return &SelectResponse{
		Name:           sub.Name,
		Namespace:      sub.Namespace,
		DisplayName:    sub.DisplayName,
		Description:    sub.Description,
		Priority:       sub.Priority,
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
// No details are included to prevent namespace enumeration.
type SubscriptionAmbiguousError struct{}

func (e *SubscriptionAmbiguousError) Error() string {
	return "subscription name is ambiguous, use qualified name 'namespace/name'"
}

// ModelNotInSubscriptionError indicates the requested model is not included in the subscription.
type ModelNotInSubscriptionError struct {
	Subscription string
	Model        string
}

func (e *ModelNotInSubscriptionError) Error() string {
	return fmt.Sprintf("subscription %s does not include model %s", e.Subscription, e.Model)
}
