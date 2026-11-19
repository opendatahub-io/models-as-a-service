package subscription

import (
	"context"
	"errors"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

// ErrSubscriptionsDisabled is returned by subscription-backed operations when
// maas-api runs without a subscription system.
var ErrSubscriptionsDisabled = errors.New("subscriptions are disabled in standalone mode")

// Backend is the full subscription surface consumed by maas-api. Both the live
// Selector and DisabledSelector implement it, so cmd/main.go can choose one
// implementation and pass it to every request handler.
type Backend interface {
	Enabled() bool
	Select(groups []string, username string, requestedSubscription string, requestedModel string) (*SelectResponse, error)
	SelectWithContext(ctx context.Context, groups []string, username string, requestedSubscription string, requestedModel string) (*SelectResponse, error)
	SelectHighestPriority(groups []string, username string) (*SelectResponse, error)
	GetAllAccessible(groups []string, username string) ([]*SelectResponse, error)
	ResolveSubscriptionAdmin(identity *token.VerifiedIdentity, requestedSubscription string) (*AdminScope, error)
	ResolveSubscriptionAdminScopes(identity *token.VerifiedIdentity) ([]*AdminScope, error)
	CanonicalizeSubscription(requestedSubscription string) (string, error)
	ListInventory(groups []string, username string, identity *token.VerifiedIdentity) ([]SubscriptionInfo, error)
	ListAccessibleForModel(username string, groups []string, modelID string) ([]SubscriptionInfo, error)
}

// DisabledSelector is the single disabled implementation of the subscription
// selection and administration contracts. It lets callers depend on a real
// implementation instead of a nil *Selector and a mode flag. Every operation
// that needs subscription state fails closed with ErrSubscriptionsDisabled.
type DisabledSelector struct{}

// Enabled reports that this backend has no subscription system.
func (DisabledSelector) Enabled() bool { return false }

func (DisabledSelector) Select(_ []string, _, _, _ string) (*SelectResponse, error) {
	return nil, ErrSubscriptionsDisabled
}

func (DisabledSelector) SelectWithContext(_ context.Context, _ []string, _, _, _ string) (*SelectResponse, error) {
	return nil, ErrSubscriptionsDisabled
}

func (DisabledSelector) SelectHighestPriority(_ []string, _ string) (*SelectResponse, error) {
	return nil, ErrSubscriptionsDisabled
}

func (DisabledSelector) GetAllAccessible(_ []string, _ string) ([]*SelectResponse, error) {
	return nil, ErrSubscriptionsDisabled
}

func (DisabledSelector) ResolveSubscriptionAdmin(_ *token.VerifiedIdentity, _ string) (*AdminScope, error) {
	return nil, &SubscriptionAdminError{
		Code:    "subscriptions_disabled",
		Message: ErrSubscriptionsDisabled.Error(),
	}
}

func (DisabledSelector) ResolveSubscriptionAdminScopes(_ *token.VerifiedIdentity) ([]*AdminScope, error) {
	return nil, &SubscriptionAdminError{
		Code:    "subscriptions_disabled",
		Message: ErrSubscriptionsDisabled.Error(),
	}
}

func (DisabledSelector) CanonicalizeSubscription(_ string) (string, error) {
	return "", &SubscriptionAdminError{
		Code:    "subscriptions_disabled",
		Message: ErrSubscriptionsDisabled.Error(),
	}
}

func (DisabledSelector) ListInventory(_ []string, _ string, _ *token.VerifiedIdentity) ([]SubscriptionInfo, error) {
	return nil, ErrSubscriptionsDisabled
}

func (DisabledSelector) ListAccessibleForModel(_ string, _ []string, _ string) ([]SubscriptionInfo, error) {
	return nil, ErrSubscriptionsDisabled
}
