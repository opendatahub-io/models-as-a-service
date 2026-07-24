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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// MaaSModelRefValidator validates MaaSModelRef resources.
// +kubebuilder:webhook:path=/validate-maas-opendatahub-io-v1alpha1-maasmodelref,mutating=false,failurePolicy=fail,sideEffects=None,groups=maas.opendatahub.io,resources=maasmodelrefs,verbs=create;update,versions=v1alpha1,name=vmaasmodelref.kb.io,admissionReviewVersions=v1

type MaaSModelRefValidator struct {
	Client            client.Reader
	AITenantNamespace string
}

// SetupWebhookWithManager registers the webhook with the manager.
func (v *MaaSModelRefValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&maasv1alpha1.MaaSModelRef{}).
		WithValidator(v).
		Complete()
}

// ValidateCreate validates MaaSModelRef on creation.
func (v *MaaSModelRefValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	model, ok := obj.(*maasv1alpha1.MaaSModelRef)
	if !ok {
		return nil, fmt.Errorf("expected MaaSModelRef object, got %T", obj)
	}

	if err := v.validateTenantRef(ctx, model); err != nil {
		return nil, err
	}

	return nil, nil
}

// ValidateUpdate validates MaaSModelRef on update.
func (v *MaaSModelRefValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	model, ok := newObj.(*maasv1alpha1.MaaSModelRef)
	if !ok {
		return nil, fmt.Errorf("expected MaaSModelRef object, got %T", newObj)
	}

	if err := v.validateTenantRef(ctx, model); err != nil {
		return nil, err
	}

	return nil, nil
}

// ValidateDelete validates MaaSModelRef on deletion.
// No validation needed for deletion.
func (v *MaaSModelRefValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// validateTenantRef checks that the referenced AITenant exists when tenantRef is set.
func (v *MaaSModelRefValidator) validateTenantRef(ctx context.Context, model *maasv1alpha1.MaaSModelRef) error {
	if model.Spec.TenantRef == "" {
		return nil
	}

	if v == nil {
		return errors.New("webhook validator not configured")
	}
	if v.Client == nil {
		return errors.New("webhook client not configured")
	}
	if v.AITenantNamespace == "" {
		return errors.New("AITenant infrastructure namespace is not configured")
	}

	aitenant := &maasv1alpha1.AITenant{}
	key := client.ObjectKey{
		Name:      model.Spec.TenantRef,
		Namespace: v.AITenantNamespace,
	}
	if err := v.Client.Get(ctx, key, aitenant); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf(
				"spec.tenantRef %q references AITenant that does not exist in namespace %s",
				model.Spec.TenantRef, v.AITenantNamespace,
			)
		}
		return fmt.Errorf("failed to validate tenantRef: %w", err)
	}

	return nil
}
