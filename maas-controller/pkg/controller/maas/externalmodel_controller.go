/*
Copyright 2025.

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

package maas

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// ExternalModelReconciler reconciles an ExternalModel object
type ExternalModelReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=externalmodels,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=externalmodels/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=externalmodels/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ExternalModelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the ExternalModel instance
	externalModel := &maasv1alpha1.ExternalModel{}
	err := r.Get(ctx, req.NamespacedName, externalModel)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Return and don't requeue
			logger.Info("ExternalModel resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		logger.Error(err, "Failed to get ExternalModel")
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling ExternalModel", "name", externalModel.Name, "namespace", externalModel.Namespace)

	// Update status fields with virtual name information
	if err := r.updateVirtualNameStatus(ctx, externalModel); err != nil {
		logger.Error(err, "Failed to update virtual name status")
		return ctrl.Result{}, err
	}

	logger.Info("Successfully reconciled ExternalModel", "name", externalModel.Name)
	return ctrl.Result{}, nil
}

// updateVirtualNameStatus populates the status fields with virtual name information
func (r *ExternalModelReconciler) updateVirtualNameStatus(ctx context.Context, externalModel *maasv1alpha1.ExternalModel) error {
	logger := log.FromContext(ctx)

	// Calculate virtual names (resource name + aliases)
	virtualNames := getVirtualNames(externalModel.Name, externalModel.Spec.ModelAliases)

	// Calculate resolved backend model name
	resolvedBackendModelName := getBackendModelName(externalModel.Spec.BackendModelName, externalModel.Name)

	// Check if status needs updating
	statusChanged := false
	if !equalStringSlices(externalModel.Status.VirtualNames, virtualNames) {
		externalModel.Status.VirtualNames = virtualNames
		statusChanged = true
	}
	if externalModel.Status.ResolvedBackendModelName != resolvedBackendModelName {
		externalModel.Status.ResolvedBackendModelName = resolvedBackendModelName
		statusChanged = true
	}

	if statusChanged {
		logger.Info("Updating ExternalModel status", 
			"virtualNames", virtualNames,
			"resolvedBackendModelName", resolvedBackendModelName)
		
		if err := r.Status().Update(ctx, externalModel); err != nil {
			return fmt.Errorf("failed to update ExternalModel status: %w", err)
		}
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ExternalModelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&maasv1alpha1.ExternalModel{}).
		Named("externalmodel-status").
		Complete(r)
}