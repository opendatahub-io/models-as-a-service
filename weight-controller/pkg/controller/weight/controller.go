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

package weight

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ExternalModel GVK for the unstructured client.
var externalModelGVK = schema.GroupVersionKind{
	Group:   "inference.opendatahub.io",
	Version: "v1alpha1",
	Kind:    "ExternalModel",
}

// Reconciler reconciles ExternalModel resources and updates weights based on cluster metrics.
type Reconciler struct {
	client.Client
	ReconcileInterval time.Duration
}

// +kubebuilder:rbac:groups=inference.opendatahub.io,resources=externalmodels,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=inference.opendatahub.io,resources=externalmodels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=inference.opendatahub.io,resources=externalproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile handles ExternalModel reconciliation.
// For each ExternalModel with multiple providers, it:
// 1. Logs the reconciliation event
// 2. (Future) Scrapes metrics from remote clusters
// 3. (Future) Calculates optimal weights
// 4. (Future) Patches ExternalModel with new weights
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("externalmodel", req.NamespacedName)

	externalModel := &unstructured.Unstructured{}
	externalModel.SetGroupVersionKind(externalModelGVK)

	if err := r.Get(ctx, req.NamespacedName, externalModel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("Reconciling ExternalModel",
		"name", externalModel.GetName(),
		"namespace", externalModel.GetNamespace(),
	)

	providerRefs, err := getExternalProviderRefs(externalModel)
	if err != nil {
		log.Error(err, "Failed to get externalProviderRefs from ExternalModel")
		return ctrl.Result{}, err
	}

	log.Info("Found providers", "providerCount", len(providerRefs))

	if len(providerRefs) < 2 {
		log.V(1).Info("ExternalModel has fewer than 2 providers, skipping weight calculation",
			"providerCount", len(providerRefs),
		)
		return ctrl.Result{}, nil
	}

	logProviderWeights(log, providerRefs)

	// TODO: Phase 2 - Scrape metrics from each provider's cluster
	// TODO: Phase 3 - Calculate optimal weights based on metrics
	// TODO: Phase 3 - Patch ExternalModel with new weights if changed

	return ctrl.Result{RequeueAfter: r.ReconcileInterval}, nil
}

// providerRef holds extracted data from an externalProviderRef entry.
type providerRef struct {
	RefName     string
	TargetModel string
	Weight      int
}

// getExternalProviderRefs extracts the externalProviderRefs slice from an unstructured ExternalModel.
func getExternalProviderRefs(obj *unstructured.Unstructured) ([]providerRef, error) {
	spec, found, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil || !found {
		return nil, fmt.Errorf("spec not found: %w", err)
	}

	refs, found, err := unstructured.NestedSlice(spec, "externalProviderRefs")
	if err != nil || !found {
		return nil, nil
	}

	result := make([]providerRef, 0, len(refs))
	for _, item := range refs {
		refMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		pr := providerRef{Weight: 1}

		if ref, ok := refMap["ref"].(map[string]interface{}); ok {
			if name, ok := ref["name"].(string); ok {
				pr.RefName = name
			}
		}

		if targetModel, ok := refMap["targetModel"].(string); ok {
			pr.TargetModel = targetModel
		}

		if weight, ok := refMap["weight"].(int64); ok {
			pr.Weight = int(weight)
		} else if weight, ok := refMap["weight"].(float64); ok {
			pr.Weight = int(weight)
		}

		result = append(result, pr)
	}

	return result, nil
}

func logProviderWeights(log logr.Logger, refs []providerRef) {
	for i, ref := range refs {
		log.V(1).Info("Provider weight",
			"index", i,
			"provider", ref.RefName,
			"targetModel", ref.TargetModel,
			"weight", ref.Weight,
		)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	externalModel := &unstructured.Unstructured{}
	externalModel.SetGroupVersionKind(externalModelGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(externalModel).
		Named("weight").
		Complete(r)
}
