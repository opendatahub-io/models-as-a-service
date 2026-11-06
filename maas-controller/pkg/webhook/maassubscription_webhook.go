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

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// Rate-limit identity values. Keep in sync with
// maas-controller/pkg/controller/maas.RateLimitIdentity*.
const (
	rateLimitIdentityShort  = "short"
	rateLimitIdentityLegacy = "legacy"
)

// MaaSSubscriptionValidator validates MaaSSubscription resources.
// +kubebuilder:webhook:path=/validate-maas-opendatahub-io-v1alpha1-maassubscription,mutating=false,failurePolicy=fail,sideEffects=None,groups=maas.opendatahub.io,resources=maassubscriptions,verbs=create;update,versions=v1alpha1,name=vmaassubscription.kb.io,admissionReviewVersions=v1

type MaaSSubscriptionValidator struct {
	Client    client.Reader
	Validator *TenantNamespaceValidator
}

// SetupWebhookWithManager registers the webhook with the manager.
func (v *MaaSSubscriptionValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &maasv1alpha1.MaaSSubscription{}).
		WithValidator(v).
		Complete()
}

// ValidateCreate validates MaaSSubscription on creation.
func (v *MaaSSubscriptionValidator) ValidateCreate(ctx context.Context, sub *maasv1alpha1.MaaSSubscription) (admission.Warnings, error) {
	if v.Validator == nil {
		return nil, errors.New("webhook validator not configured")
	}

	allowed, message := v.Validator.ValidateNamespace(ctx, sub.Namespace)
	if !allowed {
		return nil, fmt.Errorf("%s", message)
	}

	if err := validateRateLimitIdentitySpec(sub); err != nil {
		return nil, err
	}

	return nil, nil
}

// ValidateUpdate validates MaaSSubscription on update.
// Namespace cannot be changed on update (Kubernetes enforces this).
// Rejects unknown spec.rateLimitIdentity values. Explicit short↔legacy remains
// allowed (documented quota-reset opt-in). Also rejects changes to a
// previously set organization.id association.
func (v *MaaSSubscriptionValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *maasv1alpha1.MaaSSubscription) (admission.Warnings, error) {
	if err := validateRateLimitIdentitySpec(newObj); err != nil {
		return nil, err
	}

	oldOrganizationID := oldObj.Spec.OrganizationID()
	newOrganizationID := newObj.Spec.OrganizationID()
	if oldOrganizationID != "" && oldOrganizationID != newOrganizationID {
		return nil, errors.New("organization.id is immutable")
	}

	return nil, nil
}

func validateRateLimitIdentitySpec(sub *maasv1alpha1.MaaSSubscription) error {
	if sub == nil {
		return nil
	}
	switch sub.Spec.RateLimitIdentity {
	case "", rateLimitIdentityShort, rateLimitIdentityLegacy:
		return nil
	default:
		return fmt.Errorf(
			"spec.rateLimitIdentity must be %q or %q (got %q)",
			rateLimitIdentityShort, rateLimitIdentityLegacy, sub.Spec.RateLimitIdentity,
		)
	}
}

// ValidateDelete validates MaaSSubscription on deletion.
// No validation needed for deletion.
func (v *MaaSSubscriptionValidator) ValidateDelete(ctx context.Context, obj *maasv1alpha1.MaaSSubscription) (admission.Warnings, error) {
	// No validation needed for deletion
	return nil, nil
}
