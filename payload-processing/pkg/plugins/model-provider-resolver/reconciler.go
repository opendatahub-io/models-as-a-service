/*
Copyright 2026 The opendatahub.io Authors.

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

package model_provider_resolver

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// maasModelRefReconciler watches MaaSModelRef CRDs (via unstructured client)
// and updates the model store with provider and credential information.
type maasModelRefReconciler struct {
	client.Reader
	store *modelInfoStore
}

// Reconcile handles create/update/delete events for MaaSModelRef resources.
func (r *maasModelRefReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling MaaSModelRef", "name", req.Name, "namespace", req.Namespace)

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(maasModelRefGVK)

	err := r.Get(ctx, req.NamespacedName, obj)
	if errors.IsNotFound(err) {
		r.store.deleteByResource(req.NamespacedName)
		logger.Info("MaaSModelRef deleted, cleaned store", "name", req.Name)
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to get MaaSModelRef: %w", err)
	}

	if !obj.GetDeletionTimestamp().IsZero() {
		r.store.deleteByResource(req.NamespacedName)
		return ctrl.Result{}, nil
	}

	// Extract spec.modelRef fields
	kind, _, _ := unstructured.NestedString(obj.Object, "spec", "modelRef", "kind")
	if kind != "ExternalModel" {
		return ctrl.Result{}, nil
	}

	modelName, _, _ := unstructured.NestedString(obj.Object, "spec", "modelRef", "name")
	provider, _, _ := unstructured.NestedString(obj.Object, "spec", "modelRef", "provider")
	if provider == "" {
		logger.Info("MaaSModelRef ExternalModel missing provider, skipping", "name", req.Name)
		return ctrl.Result{}, nil
	}

	info := ModelInfo{
		provider: provider,
	}

	// Extract spec.credentialRef if present
	credName, _, _ := unstructured.NestedString(obj.Object, "spec", "credentialRef", "name")
	credNS, _, _ := unstructured.NestedString(obj.Object, "spec", "credentialRef", "namespace")
	if credName != "" {
		info.credentialRefName = credName
		info.credentialRefNamespace = credNS
		if info.credentialRefNamespace == "" {
			info.credentialRefNamespace = obj.GetNamespace()
		}
	}

	r.store.setModelInfo(modelName, info, req.NamespacedName)
	logger.Info("Updated model store", "model", modelName, "provider", provider)

	return ctrl.Result{}, nil
}
