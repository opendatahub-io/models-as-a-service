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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func TestExternalModelValidator_ValidateCreate(t *testing.T) {
	validator := &ExternalModelValidator{}
	model := &maasv1alpha1.ExternalModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "legacy-model",
			Namespace: "models-as-a-service",
		},
		Spec: maasv1alpha1.ExternalModelSpec{
			Provider:    "openai",
			TargetModel: "gpt-4o",
			Endpoint:    "api.openai.com",
			CredentialRef: maasv1alpha1.CredentialReference{
				Name: "creds",
			},
		},
	}

	_, err := validator.ValidateCreate(context.Background(), model)
	if err == nil {
		t.Fatal("ValidateCreate() expected error, got nil")
	}
	if err.Error() != legacyExternalModelCreateDeniedMessage {
		t.Fatalf("ValidateCreate() error = %q, want %q", err.Error(), legacyExternalModelCreateDeniedMessage)
	}
}

func TestExternalModelValidator_ValidateUpdate(t *testing.T) {
	validator := &ExternalModelValidator{}
	oldModel := &maasv1alpha1.ExternalModel{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy-model", Namespace: "models-as-a-service"},
	}
	newModel := oldModel.DeepCopy()
	newModel.Spec.TargetModel = "gpt-4o-mini"

	_, err := validator.ValidateUpdate(context.Background(), oldModel, newModel)
	if err != nil {
		t.Fatalf("ValidateUpdate() unexpected error: %v", err)
	}
}

func TestExternalModelValidator_ValidateDelete(t *testing.T) {
	validator := &ExternalModelValidator{}
	model := &maasv1alpha1.ExternalModel{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy-model", Namespace: "models-as-a-service"},
	}

	_, err := validator.ValidateDelete(context.Background(), model)
	if err != nil {
		t.Fatalf("ValidateDelete() unexpected error: %v", err)
	}
}
