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
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// AITenantValidator validates AITenant resources.
// +kubebuilder:webhook:path=/validate-maas-opendatahub-io-v1alpha1-aitenant,mutating=false,failurePolicy=fail,sideEffects=None,groups=maas.opendatahub.io,resources=aitenants,verbs=create,versions=v1alpha1,name=vaitenant.kb.io,admissionReviewVersions=v1
type AITenantValidator struct {
	AITenantNamespace string
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
	if v.AITenantNamespace == "" {
		return nil, errors.New("AITenant infrastructure namespace is not configured")
	}
	if aitenant.Namespace != v.AITenantNamespace {
		return nil, fmt.Errorf(
			"AITenant %s/%s must be created in the configured AITenant infrastructure namespace %q",
			aitenant.Namespace, aitenant.Name, v.AITenantNamespace,
		)
	}
	_ = ctx
	return nil, nil
}

// ValidateUpdate validates AITenant on update.
// Namespace is immutable and placement is enforced on create, so no update validation is needed here.
func (v *AITenantValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	_ = ctx
	_ = oldObj
	_ = newObj
	return nil, nil
}

// ValidateDelete validates AITenant on deletion.
// No validation needed for deletion.
func (v *AITenantValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	_ = ctx
	_ = obj
	return nil, nil
}
