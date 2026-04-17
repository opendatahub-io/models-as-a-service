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
	"os"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netwv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

// deletePropagation is used for child deletes so the Tenant finalizer does not block on foreground chains.
var deletePropagation = client.PropagationPolicy(metav1.DeletePropagationBackground)

// optionalPlatformGVKs are extension resources created by the legacy ODH modelsasservice pipeline (and future
// maas-controller apply) that may reference Tenant as controller owner. List failures are ignored when the
// API is not installed.
var optionalPlatformGVKs = []schema.GroupVersionKind{
	{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"},
	{Group: "kuadrant.io", Version: "v1", Kind: "RateLimitPolicy"},
	{Group: "extensions.kuadrant.io", Version: "v1alpha1", Kind: "TelemetryPolicy"},
	{Group: "networking.istio.io", Version: "v1", Kind: "DestinationRule"},
	{Group: "networking.istio.io", Version: "v1alpha3", Kind: "EnvoyFilter"},
	{Group: "telemetry.istio.io", Version: "v1", Kind: "Telemetry"},
}

func (r *TenantReconciler) operatorNamespace() string {
	if r.OperatorNamespace != "" {
		return r.OperatorNamespace
	}
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return os.Getenv("WATCH_NAMESPACE")
}

func ownedByTenantRef(obj metav1.Object, tenant *maasv1alpha1.Tenant) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == tenant.UID &&
			ref.Kind == maasv1alpha1.TenantKind &&
			ref.APIVersion == maasv1alpha1.GroupVersion.String() {
			return true
		}
	}
	return false
}

func ownedByTenantLabel(obj metav1.Object, tenant *maasv1alpha1.Tenant) bool {
	labels := obj.GetLabels()
	return labels != nil &&
		labels[tenantreconcile.LabelTenantName] == tenant.Name &&
		labels[tenantreconcile.LabelTenantNamespace] == tenant.Namespace
}

func isOwnedByTenant(obj metav1.Object, tenant *maasv1alpha1.Tenant) bool {
	return ownedByTenantRef(obj, tenant) || ownedByTenantLabel(obj, tenant)
}

func tenantWorkNamespaces(tenant *maasv1alpha1.Tenant, operatorNS, appNS string) []string {
	out := sets.New[string]()
	if tenant.Namespace != "" {
		out.Insert(tenant.Namespace)
	}
	if appNS != "" {
		out.Insert(appNS)
	}
	if operatorNS != "" {
		out.Insert(operatorNS)
	}
	if tenant.Spec.GatewayRef.Namespace != "" {
		out.Insert(tenant.Spec.GatewayRef.Namespace)
	}
	return sets.List(out)
}

// finalizeTenantDeletion deletes API objects owned by the tenant (owner refs). It returns
// (stillPending, err): stillPending means children are present or terminating — requeue without removing the finalizer.
func (r *TenantReconciler) finalizeTenantDeletion(ctx context.Context, tenant *maasv1alpha1.Tenant) (bool, error) {
	opNS := r.operatorNamespace()
	namespaces := tenantWorkNamespaces(tenant, opNS, r.AppNamespace)
	if len(namespaces) == 0 {
		return false, fmt.Errorf("cannot finalize Tenant %s/%s: no work namespaces resolved (operator namespace and GatewayRef.Namespace are both empty); namespaced children may be orphaned", tenant.Namespace, tenant.Name)
	}

	pending := false

	for _, ns := range namespaces {
		p, err := r.deleteOwnedInNamespace(ctx, tenant, ns)
		if err != nil {
			return false, err
		}
		pending = pending || p
	}

	p, err := r.deleteOwnedClusterScoped(ctx, tenant)
	if err != nil {
		return false, err
	}
	pending = pending || p

	return pending, nil
}

func (r *TenantReconciler) deleteOwnedInNamespace(ctx context.Context, tenant *maasv1alpha1.Tenant, ns string) (bool, error) {
	pending := false

	var cmList corev1.ConfigMapList
	if err := r.List(ctx, &cmList, client.InNamespace(ns)); err != nil {
		return false, fmt.Errorf("list ConfigMaps in %q: %w", ns, err)
	}
	for i := range cmList.Items {
		item := &cmList.Items[i]
		if !isOwnedByTenant(item, tenant) {
			continue
		}
		if !item.GetDeletionTimestamp().IsZero() {
			pending = true
			continue
		}
		if err := r.Delete(ctx, item, deletePropagation); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("delete ConfigMap %s/%s: %w", ns, item.Name, err)
		}
		pending = true
	}

	var svcList corev1.ServiceList
	if err := r.List(ctx, &svcList, client.InNamespace(ns)); err != nil {
		return false, fmt.Errorf("list Services in %q: %w", ns, err)
	}
	for i := range svcList.Items {
		item := &svcList.Items[i]
		if !isOwnedByTenant(item, tenant) {
			continue
		}
		if !item.GetDeletionTimestamp().IsZero() {
			pending = true
			continue
		}
		if err := r.Delete(ctx, item, deletePropagation); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("delete Service %s/%s: %w", ns, item.Name, err)
		}
		pending = true
	}

	var saList corev1.ServiceAccountList
	if err := r.List(ctx, &saList, client.InNamespace(ns)); err != nil {
		return false, fmt.Errorf("list ServiceAccounts in %q: %w", ns, err)
	}
	for i := range saList.Items {
		item := &saList.Items[i]
		if !isOwnedByTenant(item, tenant) {
			continue
		}
		if !item.GetDeletionTimestamp().IsZero() {
			pending = true
			continue
		}
		if err := r.Delete(ctx, item, deletePropagation); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("delete ServiceAccount %s/%s: %w", ns, item.Name, err)
		}
		pending = true
	}

	var depList appsv1.DeploymentList
	if err := r.List(ctx, &depList, client.InNamespace(ns)); err != nil {
		return false, fmt.Errorf("list Deployments in %q: %w", ns, err)
	}
	for i := range depList.Items {
		item := &depList.Items[i]
		if !isOwnedByTenant(item, tenant) {
			continue
		}
		if !item.GetDeletionTimestamp().IsZero() {
			pending = true
			continue
		}
		if err := r.Delete(ctx, item, deletePropagation); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("delete Deployment %s/%s: %w", ns, item.Name, err)
		}
		pending = true
	}

	var npList netwv1.NetworkPolicyList
	if err := r.List(ctx, &npList, client.InNamespace(ns)); err != nil {
		return false, fmt.Errorf("list NetworkPolicies in %q: %w", ns, err)
	}
	for i := range npList.Items {
		item := &npList.Items[i]
		if !isOwnedByTenant(item, tenant) {
			continue
		}
		if !item.GetDeletionTimestamp().IsZero() {
			pending = true
			continue
		}
		if err := r.Delete(ctx, item, deletePropagation); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("delete NetworkPolicy %s/%s: %w", ns, item.Name, err)
		}
		pending = true
	}

	var hrList gwapiv1.HTTPRouteList
	if err := r.List(ctx, &hrList, client.InNamespace(ns)); err != nil {
		return false, fmt.Errorf("list HTTPRoutes in %q: %w", ns, err)
	}
	for i := range hrList.Items {
		item := &hrList.Items[i]
		if !isOwnedByTenant(item, tenant) {
			continue
		}
		if !item.GetDeletionTimestamp().IsZero() {
			pending = true
			continue
		}
		if err := r.Delete(ctx, item, deletePropagation); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("delete HTTPRoute %s/%s: %w", ns, item.Name, err)
		}
		pending = true
	}

	for _, gvk := range optionalPlatformGVKs {
		p, err := r.deleteOwnedUnstructured(ctx, tenant, ns, gvk)
		if err != nil {
			return false, err
		}
		pending = pending || p
	}

	return pending, nil
}

func (r *TenantReconciler) deleteOwnedUnstructured(ctx context.Context, tenant *maasv1alpha1.Tenant, ns string, gvk schema.GroupVersionKind) (bool, error) {
	listGVK := gvk
	listGVK.Kind = gvk.Kind + "List"

	ul := &unstructured.UnstructuredList{}
	ul.SetGroupVersionKind(listGVK)

	if err := r.List(ctx, ul, client.InNamespace(ns)); err != nil {
		if meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, fmt.Errorf("list %s in namespace %q: %w", listGVK.String(), ns, err)
	}

	pending := false
	for i := range ul.Items {
		obj := &ul.Items[i]
		if !isOwnedByTenant(obj, tenant) {
			continue
		}
		if !obj.GetDeletionTimestamp().IsZero() {
			pending = true
			continue
		}
		if err := r.Delete(ctx, obj, deletePropagation); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("delete %s %s/%s: %w", obj.GetKind(), ns, obj.GetName(), err)
		}
		pending = true
	}
	return pending, nil
}

func (r *TenantReconciler) deleteOwnedClusterScoped(ctx context.Context, tenant *maasv1alpha1.Tenant) (bool, error) {
	pending := false

	var crList rbacv1.ClusterRoleList
	if err := r.List(ctx, &crList); err != nil {
		return false, fmt.Errorf("list ClusterRoles: %w", err)
	}
	for i := range crList.Items {
		item := &crList.Items[i]
		if !isOwnedByTenant(item, tenant) {
			continue
		}
		if !item.GetDeletionTimestamp().IsZero() {
			pending = true
			continue
		}
		if err := r.Delete(ctx, item, deletePropagation); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("delete ClusterRole %s: %w", item.Name, err)
		}
		pending = true
	}

	var crbList rbacv1.ClusterRoleBindingList
	if err := r.List(ctx, &crbList); err != nil {
		return false, fmt.Errorf("list ClusterRoleBindings: %w", err)
	}
	for i := range crbList.Items {
		item := &crbList.Items[i]
		if !isOwnedByTenant(item, tenant) {
			continue
		}
		if !item.GetDeletionTimestamp().IsZero() {
			pending = true
			continue
		}
		if err := r.Delete(ctx, item, deletePropagation); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("delete ClusterRoleBinding %s: %w", item.Name, err)
		}
		pending = true
	}

	return pending, nil
}

// finalizeRequeueInterval is used while owned children are still terminating.
const finalizeRequeueInterval = 5 * time.Second
