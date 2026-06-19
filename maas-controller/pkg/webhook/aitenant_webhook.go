/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package webhook

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// AITenantValidator validates AITenant resources.
// +kubebuilder:webhook:path=/validate-maas-opendatahub-io-v1alpha1-aitenant,mutating=false,failurePolicy=fail,sideEffects=None,groups=maas.opendatahub.io,resources=aitenants,verbs=create;update,versions=v1alpha1,name=vaitenant.kb.io,admissionReviewVersions=v1
type AITenantValidator struct {
	Client            client.Reader
	AITenantNamespace string
	GatewayNamespace  string
}

// SetupWebhookWithManager registers the webhook with the manager.
func (v *AITenantValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&maasv1alpha1.AITenant{}).
		WithValidator(v).
		Complete()
}

// ValidateCreate validates AITenant on creation.
func (v *AITenantValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	aitenant, ok := obj.(*maasv1alpha1.AITenant)
	if !ok {
		return nil, fmt.Errorf("expected AITenant object, got %T", obj)
	}
	if v == nil {
		return nil, errors.New("webhook validator not configured")
	}
	if v.Client == nil {
		return nil, errors.New("webhook client not configured")
	}
	if v.AITenantNamespace == "" {
		return nil, errors.New("AITenant infrastructure namespace is not configured")
	}
	if v.GatewayNamespace == "" {
		return nil, errors.New("gateway namespace is not configured")
	}
	if aitenant.Namespace != v.AITenantNamespace {
		return nil, fmt.Errorf(
			"AITenant %s/%s must be created in the configured AITenant infrastructure namespace %q",
			aitenant.Namespace, aitenant.Name, v.AITenantNamespace,
		)
	}

	// Check for Gateway conflicts with existing AITenants
	if err := v.validateGatewayUniqueness(ctx, aitenant, nil); err != nil {
		return nil, err
	}

	return nil, nil
}

// ValidateUpdate validates AITenant on update.
func (v *AITenantValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldAITenant, ok := oldObj.(*maasv1alpha1.AITenant)
	if !ok {
		return nil, fmt.Errorf("expected AITenant object for old, got %T", oldObj)
	}
	newAITenant, ok := newObj.(*maasv1alpha1.AITenant)
	if !ok {
		return nil, fmt.Errorf("expected AITenant object for new, got %T", newObj)
	}

	if v == nil {
		return nil, errors.New("webhook validator not configured")
	}
	if v.Client == nil {
		return nil, errors.New("webhook client not configured")
	}
	if v.AITenantNamespace == "" {
		return nil, errors.New("AITenant infrastructure namespace is not configured")
	}
	if v.GatewayNamespace == "" {
		return nil, errors.New("gateway namespace is not configured")
	}

	// Check if gateway reference changed
	oldGatewayRef := v.gatewayRefFor(oldAITenant)
	newGatewayRef := v.gatewayRefFor(newAITenant)

	if oldGatewayRef.Namespace == newGatewayRef.Namespace && oldGatewayRef.Name == newGatewayRef.Name {
		// Gateway reference unchanged, no validation needed
		return nil, nil
	}

	// Gateway reference changed, validate the new reference is not in use
	if err := v.validateGatewayUniqueness(ctx, newAITenant, oldAITenant); err != nil {
		return nil, err
	}

	return nil, nil
}

// ValidateDelete validates AITenant on deletion.
// No validation needed for deletion.
func (v *AITenantValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	_ = ctx
	_ = obj
	return nil, nil
}

// gatewayRefFor resolves the full gateway reference for an AITenant.
// This mirrors the logic in aitenant_controller.go to ensure consistent validation.
func (v *AITenantValidator) gatewayRefFor(aitenant *maasv1alpha1.AITenant) maasv1alpha1.TenantGatewayRef {
	ref := maasv1alpha1.TenantGatewayRef{
		Namespace: v.GatewayNamespace,
		Name:      aitenant.Name,
	}
	if aitenant.Spec.Gateway != nil && aitenant.Spec.Gateway.Name != "" {
		ref.Name = aitenant.Spec.Gateway.Name
	}
	return ref
}

// validateGatewayUniqueness checks that no other AITenant is using the same Gateway.
// The oldAITenant parameter is nil for creates, and set for updates to exclude self-comparison.
func (v *AITenantValidator) validateGatewayUniqueness(ctx context.Context, aitenant *maasv1alpha1.AITenant, oldAITenant *maasv1alpha1.AITenant) error {
	targetGatewayRef := v.gatewayRefFor(aitenant)

	// List all AITenant CRs in the AITenant namespace (where AITenant CRs live, e.g., ai-tenants).
	var aitenantList maasv1alpha1.AITenantList
	if err := v.Client.List(ctx, &aitenantList, client.InNamespace(v.AITenantNamespace)); err != nil {
		return fmt.Errorf("failed to list AITenants: %w", err)
	}

	for _, existingTenant := range aitenantList.Items {
		// Skip self-comparison for updates
		if existingTenant.Name == aitenant.Name && existingTenant.Namespace == aitenant.Namespace {
			continue
		}

		existingGatewayRef := v.gatewayRefFor(&existingTenant)

		if existingGatewayRef.Namespace == targetGatewayRef.Namespace &&
			existingGatewayRef.Name == targetGatewayRef.Name {
			return fmt.Errorf(
				"gateway %s/%s is already in use by AITenant %s/%s; "+
					"each AITenant requires a dedicated Gateway for isolation; "+
					"please create a new Gateway for this tenant or specify a different gateway name in spec.gateway.name",
				targetGatewayRef.Namespace, targetGatewayRef.Name,
				existingTenant.Namespace, existingTenant.Name,
			)
		}
	}

	return nil
}
