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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

const (
	// TenantNamespaceLabel is the label that marks a namespace as tenant-enabled.
	// Namespaces with this label can contain MaaSSubscription and MaaSAuthPolicy resources.
	// This label is automatically applied when a Tenant CR is created in the namespace.
	TenantNamespaceLabel = "ai-gateway.opendatahub.io/tenant"
)

// TenantNamespaceValidator validates whether a namespace is allowed to contain
// MaaS tenant resources (MaaSSubscription, MaaSAuthPolicy).
type TenantNamespaceValidator struct {
	Client client.Reader
}

// ValidateNamespace checks if the given namespace is allowed to contain MaaS tenant resources.
// Returns (allowed bool, error message string).
//
// A namespace is allowed if:
// 1. It has the TenantNamespaceLabel, AND
// 2. A Tenant CR exists in the namespace
//
// This ensures proper tenant initialization - both the namespace label and Tenant CR
// must be present before MaaSSubscription or MaaSAuthPolicy resources can be created.
func (v *TenantNamespaceValidator) ValidateNamespace(ctx context.Context, namespace string) (bool, string) {
	if v == nil || v.Client == nil {
		return false, "namespace validator not configured"
	}

	// Check if namespace exists and has the tenant label
	ns := &corev1.Namespace{}
	if err := v.Client.Get(ctx, types.NamespacedName{Name: namespace}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Sprintf("namespace %q does not exist", namespace)
		}
		return false, fmt.Sprintf("failed to validate namespace %q: %v", namespace, err)
	}

	// Check for tenant label (label key must be present; value can be anything including empty string)
	_, hasTenantLabel := ns.Labels[TenantNamespaceLabel]
	if !hasTenantLabel {
		return false, fmt.Sprintf(
			"namespace %q is not enabled for MaaS tenant resources. "+
				"Tenant namespaces must have the label %q. "+
				"To enable this namespace, create a Tenant CR which will automatically apply the label.",
			namespace, TenantNamespaceLabel,
		)
	}

	// Check if a Tenant CR exists in this namespace
	tenantList := &maasv1alpha1.TenantList{}
	if err := v.Client.List(ctx, tenantList, client.InNamespace(namespace)); err != nil {
		return false, fmt.Sprintf("failed to check for Tenant CR in namespace %q: %v", namespace, err)
	}

	if len(tenantList.Items) == 0 {
		return false, fmt.Sprintf(
			"namespace %q has the tenant label but no Tenant CR exists. "+
				"Create a Tenant CR in this namespace to complete tenant initialization.",
			namespace,
		)
	}

	return true, ""
}
