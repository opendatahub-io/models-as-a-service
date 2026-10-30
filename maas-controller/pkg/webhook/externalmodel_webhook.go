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

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

const legacyExternalModelCreateDeniedMessage = "maas.opendatahub.io ExternalModel is deprecated; " +
	"create inference.opendatahub.io ExternalProvider and ExternalModel instead " +
	"(see the External Model setup guide in the MaaS documentation)"

// ExternalModelValidator rejects creation of legacy maas.opendatahub.io ExternalModel resources.
// Updates and deletes remain allowed so existing objects can be managed during migration.
// +kubebuilder:webhook:path=/validate-maas-opendatahub-io-v1alpha1-externalmodel,mutating=false,failurePolicy=fail,sideEffects=None,groups=maas.opendatahub.io,resources=externalmodels,verbs=create,versions=v1alpha1,name=vexternalmodel.kb.io,admissionReviewVersions=v1
type ExternalModelValidator struct{}

// SetupWebhookWithManager registers the webhook with the manager.
func (v *ExternalModelValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &maasv1alpha1.ExternalModel{}).
		WithValidator(v).
		Complete()
}

// ValidateCreate rejects new legacy ExternalModel resources.
func (v *ExternalModelValidator) ValidateCreate(_ context.Context, _ *maasv1alpha1.ExternalModel) (admission.Warnings, error) {
	return nil, errors.New(legacyExternalModelCreateDeniedMessage)
}

// ValidateUpdate allows spec changes on existing legacy ExternalModel resources.
func (v *ExternalModelValidator) ValidateUpdate(_ context.Context, _ *maasv1alpha1.ExternalModel, _ *maasv1alpha1.ExternalModel) (admission.Warnings, error) {
	return nil, nil
}

// ValidateDelete allows removal of legacy ExternalModel resources.
func (v *ExternalModelValidator) ValidateDelete(_ context.Context, _ *maasv1alpha1.ExternalModel) (admission.Warnings, error) {
	return nil, nil
}
