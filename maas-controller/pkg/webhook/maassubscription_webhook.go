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

// MaaSSubscriptionValidator validates MaaSSubscription resources.
// +kubebuilder:webhook:path=/validate-maas-opendatahub-io-v1alpha1-maassubscription,mutating=false,failurePolicy=fail,sideEffects=None,groups=maas.opendatahub.io,resources=maassubscriptions,verbs=create,versions=v1alpha1,name=vmaassubscription.kb.io,admissionReviewVersions=v1

type MaaSSubscriptionValidator struct {
	Client    client.Reader
	Validator *TenantNamespaceValidator
}

// SetupWebhookWithManager registers the webhook with the manager.
func (v *MaaSSubscriptionValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&maasv1alpha1.MaaSSubscription{}).
		WithValidator(v).
		Complete()
}

// ValidateCreate validates MaaSSubscription on creation.
func (v *MaaSSubscriptionValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	sub, ok := obj.(*maasv1alpha1.MaaSSubscription)
	if !ok {
		return nil, fmt.Errorf("expected MaaSSubscription object, got %T", obj)
	}

	if v.Validator == nil {
		return nil, errors.New("webhook validator not configured")
	}

	allowed, message := v.Validator.ValidateNamespace(ctx, sub.Namespace)
	if !allowed {
		return nil, fmt.Errorf("%s", message)
	}

	return nil, nil
}

// ValidateUpdate validates MaaSSubscription on update.
// Namespace cannot be changed on update (Kubernetes enforces this), so we don't need to validate.
func (v *MaaSSubscriptionValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	// No validation needed - namespace is immutable
	return nil, nil
}

// ValidateDelete validates MaaSSubscription on deletion.
// No validation needed for deletion.
func (v *MaaSSubscriptionValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	// No validation needed for deletion
	return nil, nil
}
