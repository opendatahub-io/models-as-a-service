package maas

import (
	"context"
	"fmt"
	"strings"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// validateCELValue checks that a string is safe to interpolate into a CEL expression.
// Rejects values containing characters that could break or inject into CEL string literals.
func validateCELValue(value, fieldName string) error {
	if strings.ContainsAny(value, `"\`) {
		return fmt.Errorf("%s %q contains characters unsafe for CEL expressions (double-quote or backslash)", fieldName, value)
	}
	return nil
}

// findAllSubscriptionsForModel returns all MaaSSubscriptions that reference the given model,
// excluding subscriptions that are being deleted.
func findAllSubscriptionsForModel(ctx context.Context, c client.Reader, modelName string) ([]maasv1alpha1.MaaSSubscription, error) {
	var allSubs maasv1alpha1.MaaSSubscriptionList
	if err := c.List(ctx, &allSubs); err != nil {
		return nil, fmt.Errorf("failed to list MaaSSubscriptions: %w", err)
	}
	var result []maasv1alpha1.MaaSSubscription
	for _, s := range allSubs.Items {
		if !s.GetDeletionTimestamp().IsZero() {
			continue
		}
		for _, ref := range s.Spec.ModelRefs {
			if ref.Name == modelName {
				result = append(result, s)
				break
			}
		}
	}
	return result, nil
}

// findAllAuthPoliciesForModel returns all MaaSAuthPolicies that reference the given model,
// excluding policies that are being deleted.
func findAllAuthPoliciesForModel(ctx context.Context, c client.Reader, modelName string) ([]maasv1alpha1.MaaSAuthPolicy, error) {
	var allPolicies maasv1alpha1.MaaSAuthPolicyList
	if err := c.List(ctx, &allPolicies); err != nil {
		return nil, fmt.Errorf("failed to list MaaSAuthPolicies: %w", err)
	}
	var result []maasv1alpha1.MaaSAuthPolicy
	for _, p := range allPolicies.Items {
		if !p.GetDeletionTimestamp().IsZero() {
			continue
		}
		for _, ref := range p.Spec.ModelRefs {
			if ref == modelName {
				result = append(result, p)
				break
			}
		}
	}
	return result, nil
}

// findAnySubscriptionForModel returns any one non-deleted MaaSSubscription that references the model.
// Used by watch mappers to find a subscription to trigger reconciliation for a model.
func findAnySubscriptionForModel(ctx context.Context, c client.Reader, modelName string) *maasv1alpha1.MaaSSubscription {
	subs, err := findAllSubscriptionsForModel(ctx, c, modelName)
	if err != nil || len(subs) == 0 {
		return nil
	}
	return &subs[0]
}

// findAnyAuthPolicyForModel returns any one non-deleted MaaSAuthPolicy that references the model.
func findAnyAuthPolicyForModel(ctx context.Context, c client.Reader, modelName string) *maasv1alpha1.MaaSAuthPolicy {
	policies, err := findAllAuthPoliciesForModel(ctx, c, modelName)
	if err != nil || len(policies) == 0 {
		return nil
	}
	return &policies[0]
}
