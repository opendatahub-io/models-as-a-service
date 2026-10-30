package subscription

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

const organizationAdminRole = "admins"

// SubscriptionAdminError carries a bounded callback error code and message.
type SubscriptionAdminError struct {
	Code    string
	Message string
}

func (e *SubscriptionAdminError) Error() string {
	return e.Message
}

// ResolveSubscriptionAdmin resolves one explicit subscription and evaluates the
// verified organization-admin predicate. It intentionally does not invoke
// inference selection, model-access checks, readiness checks, or SAR checks.
func (s *Selector) ResolveSubscriptionAdmin(identity *token.VerifiedIdentity, requestedSubscription string) (*AdminScope, error) {
	if identity == nil {
		return nil, &SubscriptionAdminError{
			Code:    "invalid_identity",
			Message: "trusted identity is required",
		}
	}
	target, err := s.adminSubscriptionTarget(requestedSubscription)
	if err != nil {
		return nil, err
	}

	if target.AssociatedOrganizationID == "" {
		return nil, &SubscriptionAdminError{
			Code:    "unresolved_association",
			Message: "subscription organization association is required",
		}
	}
	organizationID, err := canonicalOrganizationID(target.AssociatedOrganizationID)
	if err != nil {
		return nil, &SubscriptionAdminError{
			Code:    "unresolved_association",
			Message: "subscription organization association is invalid",
		}
	}
	if !identityHasOrganizationAdmin(identity, organizationID) {
		return nil, &SubscriptionAdminError{
			Code:    "access_denied",
			Message: "organization administration is denied",
		}
	}
	if s.tenantName == "" {
		return nil, &SubscriptionAdminError{
			Code:    "internal_error",
			Message: "tenant is not configured",
		}
	}

	return &AdminScope{
		Tenant:         s.tenantName,
		Namespace:      target.Namespace,
		Subscription:   target.Name,
		OrganizationID: organizationID,
	}, nil
}

func (s *Selector) CanonicalizeSubscription(requestedSubscription string) (string, error) {
	target, err := s.adminSubscriptionTarget(requestedSubscription)
	if err != nil {
		return "", err
	}
	return target.Name, nil
}

func (s *Selector) adminSubscriptionTarget(requestedSubscription string) (*subscription, error) {
	if requestedSubscription == "" {
		return nil, &SubscriptionAdminError{
			Code:    "bad_request",
			Message: "requestedSubscription is required",
		}
	}

	namespace, name, qualified, err := parseSubscriptionTarget(requestedSubscription)
	if err != nil {
		return nil, &SubscriptionAdminError{
			Code:    "bad_request",
			Message: "requestedSubscription is invalid",
		}
	}

	subscriptions, err := s.loadAdminSubscriptions()
	if err != nil {
		return nil, fmt.Errorf("failed to load subscriptions for administration: %w", err)
	}

	var matches []subscription
	for _, candidate := range subscriptions {
		if qualified {
			if candidate.Namespace == namespace && candidate.Name == name {
				matches = append(matches, candidate)
			}
			continue
		}
		if candidate.Name == name {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		return nil, &SubscriptionAdminError{
			Code:    "not_found",
			Message: "requested subscription was not found",
		}
	}
	if len(matches) > 1 {
		return nil, &SubscriptionAdminError{
			Code:    "ambiguous_subscription",
			Message: "requested subscription name is ambiguous",
		}
	}

	return &matches[0], nil
}

func (s *Selector) loadAdminSubscriptions() ([]subscription, error) {
	objects, err := s.lister.List()
	if err != nil {
		return nil, err
	}

	subscriptions := make([]subscription, 0, len(objects))
	for _, object := range objects {
		if s.subscriptionNamespace != "" && object.GetNamespace() != s.subscriptionNamespace {
			continue
		}
		parsed, err := parseSubscription(object)
		if err != nil {
			s.logger.Warn("Failed to parse subscription for administration, skipping",
				"name", object.GetName(),
				"namespace", object.GetNamespace(),
				"error", err,
			)
			continue
		}
		subscriptions = append(subscriptions, parsed)
	}
	return subscriptions, nil
}

func parseSubscriptionTarget(requestedSubscription string) (string, string, bool, error) {
	if requestedSubscription == "" || strings.TrimSpace(requestedSubscription) != requestedSubscription {
		return "", "", false, errors.New("subscription target is empty or padded")
	}
	if strings.Count(requestedSubscription, "/") > 1 {
		return "", "", false, errors.New("subscription target has too many slashes")
	}

	namespace, name, qualified := strings.Cut(requestedSubscription, "/")
	if qualified && (namespace == "" || name == "") {
		return "", "", false, errors.New("subscription target has an empty namespace or name")
	}
	if !qualified {
		name = namespace
		namespace = ""
	}
	return namespace, name, qualified, nil
}

func canonicalOrganizationID(organizationID string) (string, error) {
	parsed, err := uuid.Parse(organizationID)
	if err != nil || parsed.String() != organizationID {
		return "", errors.New("organization id is not a canonical UUID")
	}
	return parsed.String(), nil
}

func identityHasOrganizationAdmin(identity *token.VerifiedIdentity, organizationID string) bool {
	if identity == nil || identity.Source != "oidc" {
		return false
	}
	for _, membership := range identity.Claims.Organizations {
		if membership.OrganizationID == organizationID && slices.Contains(membership.Roles, organizationAdminRole) {
			return true
		}
	}
	return false
}
