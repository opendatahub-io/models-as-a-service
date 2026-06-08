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

	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// TenantNamespaceValidator validates whether a namespace is allowed to contain
// MaaS tenant resources (MaaSSubscription, MaaSAuthPolicy).
type TenantNamespaceValidator struct {
	Client client.Reader
}

// ValidateNamespace checks if the given namespace is allowed to contain MaaS tenant resources.
// Returns (allowed bool, error message string).
//
// A namespace is allowed if a Tenant CR exists in the namespace.
// This ensures proper tenant initialization before MaaSSubscription or MaaSAuthPolicy
// resources can be created.
func (v *TenantNamespaceValidator) ValidateNamespace(ctx context.Context, namespace string) (bool, string) {
	if v == nil || v.Client == nil {
		return false, "namespace validator not configured"
	}

	// Check if a Tenant CR exists in this namespace
	tenantList := &maasv1alpha1.TenantList{}
	if err := v.Client.List(ctx, tenantList, client.InNamespace(namespace)); err != nil {
		return false, fmt.Sprintf("failed to check for Tenant CR in namespace %q: %v", namespace, err)
	}

	if len(tenantList.Items) == 0 {
		return false, fmt.Sprintf(
			"namespace %q is not enabled for MaaS tenant resources. "+
				"Create a Tenant CR in this namespace to enable it.",
			namespace,
		)
	}

	return true, ""
}
