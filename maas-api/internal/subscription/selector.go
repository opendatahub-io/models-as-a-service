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
	Groups         []string
	Users          []string
	Priority       int32
	MaxLimit       int64
	OrganizationID string
	CostCenter     string
	Labels         map[string]string
}

// Select implements the subscription selection logic.
// Returns the selected subscription or an error if none found.
func (s *Selector) Select(groups []string, username string, requestedSubscription string) (*SelectResponse, error) {
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
	if requestedSubscription != "" {
		for _, sub := range subscriptions {
			if sub.Name == requestedSubscription {
				if userHasAccess(&sub, username, groups) {
					return toResponse(&sub), nil
				}
				return nil, &AccessDeniedError{Subscription: requestedSubscription}
			}
		}
		return nil, &SubscriptionNotFoundError{Subscription: requestedSubscription}
	}

	// Branch 2: Auto-selection (only if user has exactly one subscription)
	var accessibleSubs []subscription
	for _, sub := range subscriptions {
		if userHasAccess(&sub, username, groups) {
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
		Name: obj.GetName(),
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

	// Parse modelRefs to calculate maxLimit
	if modelRefs, found, _ := unstructured.NestedSlice(spec, "modelRefs"); found {
		for _, modelRef := range modelRefs {
			if modelMap, ok := modelRef.(map[string]any); ok {
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
	return &SelectResponse{
		Name:           sub.Name,
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
	return fmt.Sprintf("subscription %q not found", e.Subscription)
}

// AccessDeniedError indicates user doesn't have access to requested subscription.
type AccessDeniedError struct {
	Subscription string
}

func (e *AccessDeniedError) Error() string {
	return fmt.Sprintf("access denied to subscription %q", e.Subscription)
}

// MultipleSubscriptionsError indicates user has access to multiple subscriptions and must explicitly select one.
type MultipleSubscriptionsError struct {
	Subscriptions []string
}

func (e *MultipleSubscriptionsError) Error() string {
	return fmt.Sprintf("user has access to multiple subscriptions [%s], must specify subscription using X-MaaS-Subscription header", strings.Join(e.Subscriptions, ", "))
}
