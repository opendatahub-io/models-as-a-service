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

// Phase constants for MaaSSubscription status.
// These must match the Phase values defined in maas-controller/api/maas/v1alpha1/common_types.go.
const (
	PhasePending  = "Pending"
	PhaseActive   = "Active"
	PhaseDegraded = "Degraded"
	PhaseFailed   = "Failed"
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
	Name                   string
	Namespace              string
	DisplayName            string
	Description            string
	Groups                 []string
	Users                  []string
	Priority               int32
	MaxLimit               int64
	OrganizationID         string
	CostCenter             string
	Labels                 map[string]string
	ModelRefs              []ModelRefInfo
	Phase                  string                 // status.phase: "Active", "Failed", "Pending", or ""
	Ready                  bool                   // computed from status.conditions Ready condition
	DeletionTimestamp      *string                // metadata.deletionTimestamp (set when being deleted)
	TokenRateLimitStatuses []TokenRateLimitStatus // per-model TRLP status from status.tokenRateLimitStatuses
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

	accessible := make([]*SelectResponse, 0, len(subscriptions))
	for _, sub := range subscriptions {
		// Check user access
		if !userHasAccess(&sub, username, groups) {
			continue
		}

		// Allowlist: only include Active and Degraded subscriptions
		// Exclude Failed, Pending, empty (unreconciled), unknown phases, and deleting subscriptions
		if sub.Phase != PhaseActive && sub.Phase != PhaseDegraded {
			continue
		}

		// Exclude subscriptions being deleted
		if sub.DeletionTimestamp != nil {
			continue
		}

		accessible = append(accessible, toResponse(&sub))
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
				// Check model health for Degraded subscriptions
				if err := checkModelHealth(&sub, requestedModel); err != nil {
					return nil, err
				}
				return toResponse(&sub), nil
			}
		}

		// If no qualified match found and request is bare name (no '/'), try bare name matching
		if !strings.Contains(requestedSubscription, "/") {
			for _, sub := range subscriptions {
				if sub.Name != requestedSubscription {
					continue
				}
				if !userHasAccess(&sub, username, groups) {
					return nil, &AccessDeniedError{Subscription: requestedSubscription}
				}
				if requestedModel != "" && !subscriptionIncludesModel(&sub, requestedModel) {
					return nil, &ModelNotInSubscriptionError{Subscription: requestedSubscription, Model: requestedModel}
				}
				// Check model health for Degraded subscriptions
				if err := checkModelHealth(&sub, requestedModel); err != nil {
					return nil, err
				}
				return toResponse(&sub), nil
			}
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
		// Check model health for Degraded subscriptions
		if err := checkModelHealth(&accessibleSubs[0], requestedModel); err != nil {
			return nil, err
		}
		return toResponse(&accessibleSubs[0]), nil
	}

	// User has multiple subscriptions - require explicit selection
	subNames := make([]string, len(accessibleSubs))
	for i, sub := range accessibleSubs {
		subNames[i] = sub.Name
	}
	return nil, &MultipleSubscriptionsError{Subscriptions: subNames}
}

// SelectHighestPriority returns the accessible subscription with highest spec.priority
// (then max token limit desc, then name asc for deterministic ties).
func (s *Selector) SelectHighestPriority(groups []string, username string) (*SelectResponse, error) {
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

	var accessible []subscription
	for _, sub := range subscriptions {
		if userHasAccess(&sub, username, groups) {
			accessible = append(accessible, sub)
		}
	}

	if len(accessible) == 0 {
		return nil, &NoSubscriptionError{}
	}

	sortSubscriptionsByPriority(accessible)
	return toResponse(&accessible[0]), nil
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
//
//nolint:gocyclo // TODO: refactor to reduce cyclomatic complexity
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

	// Parse status.phase with validation
	if status, found, _ := unstructured.NestedMap(obj.Object, "status"); found {
		if phase, ok := status["phase"].(string); ok {
			// Normalize whitespace and validate against known phases
			phase = strings.TrimSpace(phase)
			switch phase {
			case PhaseActive, PhaseDegraded, PhaseFailed, PhasePending:
				sub.Phase = phase
			default:
				// Unknown phase value - keep raw for debugging but will be rejected by health checks
				sub.Phase = phase
			}
		}

		// Parse status.conditions to extract Ready condition
		if conditions, found, _ := unstructured.NestedSlice(status, "conditions"); found {
			for _, condRaw := range conditions {
				if condMap, ok := condRaw.(map[string]any); ok {
					condType, _ := condMap["type"].(string)
					if condType == "Ready" {
						condStatus, _ := condMap["status"].(string)
						sub.Ready = condStatus == "True"
						break
					}
				}
			}
		}

		// Parse status.tokenRateLimitStatuses to extract TRLP health
		if trlpStatuses, found, _ := unstructured.NestedSlice(status, "tokenRateLimitStatuses"); found {
			for _, statusRaw := range trlpStatuses {
				if statusMap, ok := statusRaw.(map[string]any); ok {
					trlpStatus := TokenRateLimitStatus{}
					if model, ok := statusMap["model"].(string); ok {
						trlpStatus.Model = model
					}
					if name, ok := statusMap["name"].(string); ok {
						trlpStatus.Name = name
					}
					if namespace, ok := statusMap["namespace"].(string); ok {
						trlpStatus.Namespace = namespace
					}
					if ready, ok := statusMap["ready"].(bool); ok {
						trlpStatus.Ready = ready
					}
					if reason, ok := statusMap["reason"].(string); ok {
						trlpStatus.Reason = reason
					}
					if message, ok := statusMap["message"].(string); ok {
						trlpStatus.Message = message
					}
					sub.TokenRateLimitStatuses = append(sub.TokenRateLimitStatuses, trlpStatus)
				}
			}
		}
	}

	// Parse metadata.deletionTimestamp
	if metadata := obj.Object["metadata"]; metadata != nil {
		if metadataMap, ok := metadata.(map[string]any); ok {
			if deletionTimestamp, ok := metadataMap["deletionTimestamp"].(string); ok && deletionTimestamp != "" {
				sub.DeletionTimestamp = &deletionTimestamp
			}
		}
	}

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

// checkModelHealth validates subscription phase and model health.
// Returns error if subscription is not in Active/Degraded phase or if model is unhealthy in Degraded subscriptions.
//
// Two validation paths:
// 1. API key creation (requestedModel=""): Allow Active/Degraded/Pending, block Failed/unreconciled.
// Rationale: Users can create keys while subscription is setting up (Pending), but enforcement
// happens at inference time. Failed subscriptions blocked to prevent key spam on broken subscriptions.
// 2. Inference (requestedModel set): Strict allowlist of Active/Degraded only.
// Blocks Pending/Failed/unreconciled at authorization time.
func checkModelHealth(sub *subscription, requestedModel string) error {
	// API key creation path: Allow Active, Degraded, Pending
	// Block Failed (prevents key spam on permanently broken subscriptions)
	// Block unreconciled (empty phase)
	if requestedModel == "" {
		if sub.Phase == "" {
			return &ModelUnhealthyError{
				Subscription: sub.Name,
				Phase:        sub.Phase,
				Reason:       "SubscriptionNotReady",
				Message:      "subscription is unreconciled (no status.phase set)",
			}
		}
		if sub.Phase == PhaseFailed {
			return &ModelUnhealthyError{
				Subscription: sub.Name,
				Phase:        sub.Phase,
				Reason:       "SubscriptionNotReady",
				Message:      "subscription is in Failed phase (cannot create API keys)",
			}
		}
		return nil // Allow Active, Degraded, Pending for API key creation
	}

	// Inference path: Allowlist only Active and Degraded subscriptions
	// Reject Failed, Pending, unreconciled, and unknown phases
	if sub.Phase != PhaseActive && sub.Phase != PhaseDegraded {
		phaseDisplay := sub.Phase
		if phaseDisplay == "" {
			phaseDisplay = "unreconciled"
		}
		return &ModelUnhealthyError{
			Subscription: sub.Name,
			Phase:        sub.Phase,
			Reason:       "SubscriptionNotReady",
			Message:      fmt.Sprintf("subscription is in %s phase (allowed: Active, Degraded)", phaseDisplay),
		}
	}

	// Active subscriptions are allowed without TRLP checks (already validated above)
	if sub.Phase != PhaseDegraded {
		return nil
	}

	// For Degraded subscriptions, verify rate limits can be enforced (if defined)
	// Parse the requested model (format: "namespace/name")
	parts := strings.SplitN(requestedModel, "/", 2)
	if len(parts) != 2 {
		return &ModelUnhealthyError{
			Subscription: sub.Name,
			Phase:        sub.Phase,
			Reason:       "InvalidModelFormat",
			Message:      "invalid model format: must be namespace/name",
		}
	}
	requestedNS := parts[0]
	requestedName := parts[1]

	// Check if this model has tokenRateLimits defined in the subscription spec
	hasRateLimits := false
	for _, ref := range sub.ModelRefs {
		if ref.Namespace == requestedNS && ref.Name == requestedName {
			if len(ref.TokenRateLimits) > 0 {
				hasRateLimits = true
			}
			break
		}
	}

	// If model doesn't have rate limits defined, allow inference (no TRLP to check)
	if !hasRateLimits {
		return nil
	}

	// Model has rate limits defined - verify TRLP is ready
	for _, trlp := range sub.TokenRateLimitStatuses {
		if trlp.Model == requestedName {
			if !trlp.Ready {
				return &ModelUnhealthyError{
					Subscription: sub.Name,
					Phase:        sub.Phase,
					Reason:       "RateLimitNotEnforced",
					Message:      "subscription rate limiting policies are not ready",
				}
			}
			// TRLP is ready - allow inference
			return nil
		}
	}

	// Model has rate limits defined but TRLP status missing - fail closed
	return &ModelUnhealthyError{
		Subscription: sub.Name,
		Phase:        sub.Phase,
		Reason:       "RateLimitNotEnforced",
		Message:      "subscription rate limiting policies are not ready",
	}
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

// sortSubscriptionsByPriority sorts in-place by priority desc, then maxLimit desc, then name asc.
func sortSubscriptionsByPriority(subs []subscription) {
	sort.SliceStable(subs, func(i, j int) bool {
		if subs[i].Priority != subs[j].Priority {
			return subs[i].Priority > subs[j].Priority
		}
		if subs[i].MaxLimit != subs[j].MaxLimit {
			return subs[i].MaxLimit > subs[j].MaxLimit
		}
		return subs[i].Name < subs[j].Name
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
	info := SubscriptionInfo{
		SubscriptionIDHeader:    sub.Name,
		SubscriptionDescription: desc,
		DisplayName:             sub.DisplayName,
		Priority:                sub.Priority,
		ModelRefs:               modelRefs,
		OrganizationID:          sub.OrganizationID,
		CostCenter:              sub.CostCenter,
		Labels:                  sub.Labels,
	}
	return info
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
	resp := &SelectResponse{
		Name:           sub.Name,
		Namespace:      sub.Namespace,
		DisplayName:    sub.DisplayName,
		Description:    sub.Description,
		Priority:       sub.Priority,
		ModelRefs:      modelRefs,
		OrganizationID: sub.OrganizationID,
		CostCenter:     sub.CostCenter,
		Labels:         sub.Labels,
		Phase:          sub.Phase,
		Ready:          sub.Ready,
	}
	if sub.DeletionTimestamp != nil {
		resp.DeletionTimestamp = *sub.DeletionTimestamp
	}
	return resp
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

// ModelNotInSubscriptionError indicates the requested model is not included in the subscription.
type ModelNotInSubscriptionError struct {
	Subscription string
	Model        string
}

func (e *ModelNotInSubscriptionError) Error() string {
	return fmt.Sprintf("subscription %s does not include model %s", e.Subscription, e.Model)
}

// ModelUnhealthyError indicates the requested model is not healthy in a Degraded subscription.
// Note: Model field is intentionally omitted to prevent XSS attacks.
type ModelUnhealthyError struct {
	Subscription string
	Phase        string // Subscription phase for Authorino OPA evaluation
	Reason       string
	Message      string
}

func (e *ModelUnhealthyError) Error() string {
	return "requested model is unhealthy in subscription"
}
