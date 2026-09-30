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

package maas

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

// tenantRenderedNames lists, per kind, the name helpers the platform pipeline renames
// operands with (patchResource and PostRender). Each yields the base name for the
// default tenant and "<base>-<tenantID>" for any other. A name no helper produces is
// rendered identically for every tenant.
var tenantRenderedNames = map[schema.GroupKind][]func(tenantID string) string{
	tenantreconcile.GVKDeployment.GroupKind(): {
		tenantreconcile.MaaSAPIDeploymentName,
		tenantreconcile.PayloadProcessingDeploymentName,
		tenantreconcile.PayloadPreProcessingDeploymentName,
	},
	tenantreconcile.GVKService.GroupKind(): {
		tenantreconcile.MaaSAPIServiceName,
		tenantreconcile.PayloadProcessingServiceName,
		tenantreconcile.PayloadPreProcessingServiceName,
	},
	tenantreconcile.GVKServiceAccount.GroupKind():     {tenantreconcile.PayloadProcessingServiceAccountName},
	tenantreconcile.GVKConfigMap.GroupKind():          {tenantreconcile.PayloadProcessingPluginsConfigMapForTenant},
	tenantreconcile.GVKClusterRoleBinding.GroupKind(): {tenantreconcile.PayloadProcessingReaderClusterRoleBindingNameForTenant},
	tenantreconcile.GVKCronJob.GroupKind():            {tenantreconcile.MaaSAPIKeyCleanupCronJobName},
	tenantreconcile.GVKHPA.GroupKind():                {tenantreconcile.PayloadProcessingHPAName},
	tenantreconcile.GVKNetworkPolicy.GroupKind():      {tenantreconcile.PayloadProcessingNetworkPolicyName},
	tenantreconcile.GVKHTTPRoute.GroupKind():          {tenantreconcile.MaaSAPIRouteName},
	tenantreconcile.GVKTokenRateLimitPolicy.GroupKind(): {
		tenantreconcile.GatewayTokenRateLimitDefaultDenyPolicyName,
	},
	tenantreconcile.GVKDestinationRule.GroupKind(): {
		tenantreconcile.GatewayDestinationRuleName,
		tenantreconcile.PayloadProcessingDeploymentName,
		tenantreconcile.PayloadPreProcessingDeploymentName,
	},
	tenantreconcile.GVKEnvoyFilter.GroupKind(): {
		tenantreconcile.PayloadProcessingEnvoyFilterName,
		tenantreconcile.UsageLogsEnvoyFilterName,
	},
	tenantreconcile.GVKTelemetryPolicy.GroupKind(): {tenantreconcile.TelemetryPolicyName},
	tenantreconcile.GVKIstioTelemetry.GroupKind():  {tenantreconcile.IstioTelemetryName},
	tenantreconcile.GVKCertificate.GroupKind():     {tenantreconcile.MaaSAPIServingCertName},
}

// isSharedTenantOperand reports whether every tenant applies obj under the same name, so
// each apply rewrites its tenant tracking labels and a label change alone says nothing
// about drift. obj is tenant-specific, and not shared, when one of its kind's name
// helpers renders its name for the tenant its tracking labels name.
//
// The default tenant renders base names. Its tracking name is the default AITenant name,
// or the tenant config's own name when the config has no AITenant labels, so both count
// as an empty tenant identifier. That also claims the base name of every renamed kind for
// the default tenant, which is correct: other tenants render suffixed names.
//
// Remaining gap: a tenant whose identifier equals the suffix of a shared name, such as a
// tenant "metrics" and the shared "maas-api-metrics" Service, makes that object look
// tenant-specific while that tenant's labels are on it.
func isSharedTenantOperand(gk schema.GroupKind, obj client.Object) bool {
	owner := obj.GetLabels()[tenantreconcile.LabelTenantName]
	tenantIDs := []string{owner}
	switch owner {
	case "", tenantreconcile.DefaultAITenantName, maasv1alpha1.MaasTenantConfigInstanceName:
		tenantIDs = append(tenantIDs, "")
	}
	for _, render := range tenantRenderedNames[gk] {
		for _, tenantID := range tenantIDs {
			if render(tenantID) == obj.GetName() {
				return false
			}
		}
	}
	return true
}
