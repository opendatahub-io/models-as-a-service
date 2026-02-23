package maas

import (
	"context"
	"errors"
	"fmt"
	"strings"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

var ErrModelNotFound = errors.New("MaaSModel not found")

// findHTTPRouteForModel is a shared helper used by MaaSAuthPolicy and MaaSSubscription controllers.
// It finds the MaaSModel by name, determines the HTTPRoute name and namespace based on the model
// kind (llmisvc or ExternalModel), and verifies the HTTPRoute exists.
// Returns (httpRouteName, httpRouteNamespace, error).
func findHTTPRouteForModel(ctx context.Context, c client.Reader, defaultNS, modelName string) (string, string, error) {
	maasModelList := &maasv1alpha1.MaaSModelList{}
	if err := c.List(ctx, maasModelList); err != nil {
		return "", "", fmt.Errorf("failed to list MaaSModels: %w", err)
	}

	// Find matching MaaSModel (prefer defaultNS, skip models being deleted)
	var maasModel *maasv1alpha1.MaaSModel
	for i := range maasModelList.Items {
		if maasModelList.Items[i].Name == modelName {
			if !maasModelList.Items[i].GetDeletionTimestamp().IsZero() {
				continue
			}
			if maasModelList.Items[i].Namespace == defaultNS {
				maasModel = &maasModelList.Items[i]
				break
			}
			if maasModel == nil {
				maasModel = &maasModelList.Items[i]
			}
		}
	}

	if maasModel == nil {
		return "", "", fmt.Errorf("%w: %s", ErrModelNotFound, modelName)
	}

	var httpRouteName string
	httpRouteNS := maasModel.Namespace
	if maasModel.Spec.ModelRef.Namespace != "" {
		httpRouteNS = maasModel.Spec.ModelRef.Namespace
	}

	switch maasModel.Spec.ModelRef.Kind {
	case "LLMInferenceService":
		llmisvcNS := maasModel.Namespace
		if maasModel.Spec.ModelRef.Namespace != "" {
			llmisvcNS = maasModel.Spec.ModelRef.Namespace
		}
		routeList := &gatewayapiv1.HTTPRouteList{}
		labelSelector := client.MatchingLabels{
			"app.kubernetes.io/name":      maasModel.Spec.ModelRef.Name,
			"app.kubernetes.io/component": "llminferenceservice-router",
			"app.kubernetes.io/part-of":   "llminferenceservice",
		}
		if err := c.List(ctx, routeList, client.InNamespace(llmisvcNS), labelSelector); err != nil {
			return "", "", fmt.Errorf("failed to list HTTPRoutes for LLMInferenceService %s: %w", maasModel.Spec.ModelRef.Name, err)
		}
		if len(routeList.Items) == 0 {
			return "", "", fmt.Errorf("HTTPRoute not found for LLMInferenceService %s in namespace %s", maasModel.Spec.ModelRef.Name, llmisvcNS)
		}
		httpRouteName = routeList.Items[0].Name
		httpRouteNS = routeList.Items[0].Namespace
	case "ExternalModel":
		httpRouteName = fmt.Sprintf("maas-model-%s", maasModel.Name)
	default:
		return "", "", fmt.Errorf("unknown model kind: %s", maasModel.Spec.ModelRef.Kind)
	}

	httpRoute := &gatewayapiv1.HTTPRoute{}
	key := client.ObjectKey{Name: httpRouteName, Namespace: httpRouteNS}
	if err := c.Get(ctx, key, httpRoute); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", fmt.Errorf("HTTPRoute %s/%s not found for model %s", httpRouteNS, httpRouteName, modelName)
		}
		return "", "", fmt.Errorf("failed to get HTTPRoute %s/%s: %w", httpRouteNS, httpRouteName, err)
	}

	return httpRouteName, httpRouteNS, nil
}

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
