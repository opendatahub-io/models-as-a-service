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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// TenantNamespaceLabel is the label that marks a namespace as tenant-enabled.
	// Namespaces with this label can contain MaaSSubscription and MaaSAuthPolicy resources.
	// This label is automatically applied by maas-controller when creating tenant namespaces.
	TenantNamespaceLabel = "ai-gateway.opendatahub.io/tenant"

	// DefaultTenantNamespace is the namespace that is always allowed for backward compatibility.
	// This is the single-tenant default namespace and is always valid regardless of labels.
	// Both fresh installs and upgrades use this namespace for the default tenant.
	DefaultTenantNamespace = "models-as-a-service"
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
// 1. It matches the DefaultTenantNamespace (backward compatibility), OR
// 2. It has the TenantNamespaceLabel (multi-tenant namespaces)
//
// This approach ensures:
//   - Backward compatibility: models-as-a-service always works (single-tenant and default tenant)
//   - Multi-tenancy: ai-tenant-default and other tenant namespaces work (via label)
//   - Security: random namespaces without the label are rejected
func (v *TenantNamespaceValidator) ValidateNamespace(ctx context.Context, namespace string) (bool, string) {
	if v == nil || v.Client == nil {
		return false, "namespace validator not configured"
	}

	// Always allow the default tenant namespace for backward compatibility.
	// This handles both single-tenant deployments and the default tenant in multi-tenant mode.
	if namespace == DefaultTenantNamespace {
		return true, ""
	}

	// For all other namespaces, check if they have the tenant label.
	// This includes ai-tenant-default (created during upgrade) and other tenant namespaces.
	ns := &corev1.Namespace{}
	if err := v.Client.Get(ctx, types.NamespacedName{Name: namespace}, ns); err != nil {
		return false, fmt.Sprintf("failed to validate namespace %q: %v", namespace, err)
	}

	// Check for tenant label
	if _, hasTenantLabel := ns.Labels[TenantNamespaceLabel]; hasTenantLabel {
		return true, ""
	}

	// Namespace is not tenant-enabled - reject with helpful error message
	return false, fmt.Sprintf(
		"namespace %q is not enabled for MaaS tenant resources. "+
			"Tenant namespaces must have the label %q, or use the default namespace %q. "+
			"To enable this namespace, add the label: kubectl label namespace %s %s=",
		namespace, TenantNamespaceLabel, DefaultTenantNamespace, namespace, TenantNamespaceLabel,
	)
}
