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
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// MaaSAuthPolicyValidator validates MaaSAuthPolicy resources.
// +kubebuilder:webhook:path=/validate-maas-opendatahub-io-v1alpha1-maasauthpolicy,mutating=false,failurePolicy=fail,sideEffects=None,groups=maas.opendatahub.io,resources=maasauthpolicies,verbs=create,versions=v1alpha1,name=vmaasauthpolicy.kb.io,admissionReviewVersions=v1

type MaaSAuthPolicyValidator struct {
	Client    client.Reader
	Validator *TenantNamespaceValidator
}

// SetupWebhookWithManager registers the webhook with the manager.
func (v *MaaSAuthPolicyValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&maasv1alpha1.MaaSAuthPolicy{}).
		WithValidator(v).
		Complete()
}

// ValidateCreate validates MaaSAuthPolicy on creation.
func (v *MaaSAuthPolicyValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	policy, ok := obj.(*maasv1alpha1.MaaSAuthPolicy)
	if !ok {
		return nil, fmt.Errorf("expected MaaSAuthPolicy object, got %T", obj)
	}

	if v.Validator == nil {
		return nil, fmt.Errorf("webhook validator not configured")
	}

	allowed, message := v.Validator.ValidateNamespace(ctx, policy.Namespace)
	if !allowed {
		return nil, fmt.Errorf("%s", message)
	}

	return nil, nil
}

// ValidateUpdate validates MaaSAuthPolicy on update.
// Namespace cannot be changed on update (Kubernetes enforces this), so we don't need to validate.
func (v *MaaSAuthPolicyValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	// No validation needed - namespace is immutable
	return nil, nil
}

// ValidateDelete validates MaaSAuthPolicy on deletion.
// No validation needed for deletion.
func (v *MaaSAuthPolicyValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	// No validation needed for deletion
	return nil, nil
}
