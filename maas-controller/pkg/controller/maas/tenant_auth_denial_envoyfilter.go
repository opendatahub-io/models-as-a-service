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
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

// Authorino's authorization phase always denies with 403. This filter runs
// before the gateway auth filter so it can translate only denials carrying the
// validation result marker. It is deployed independently of the IPP/Praxis
// pipeline and of usage logging.
const apiKeyDenialLua = `
local marker = "x-maas-api-key-denial"

function envoy_on_request(handle)
  -- A caller must not be able to supply the marker to Authorino.
  handle:headers():remove(marker)
end

function envoy_on_response(handle)
  local headers = handle:headers()
  local denial = headers:get(marker)
  headers:remove(marker)
  if headers:get(":status") ~= "403" then
    return
  end

  local status, message, error_type
  if denial == "invalid" then
    status = "401"
    message = "Invalid API key"
    error_type = "authentication_error"
  elseif denial == "unavailable" then
    status = "503"
    message = "API key validation unavailable"
    error_type = "server_error"
  else
    return
  end

  local body = '{"error":{"message":"' .. message .. '","type":"' .. error_type .. '","code":' .. status .. '}}'
  handle:body(true):setBytes(body)
  headers:replace(":status", status)
  headers:replace("content-type", "application/json")
  headers:replace("content-length", tostring(#body))
  headers:replace("x-ext-auth-reason", message)
end
`

func authDenialFilterPatch(anchor string) map[string]any {
	return map[string]any{
		"applyTo": "HTTP_FILTER",
		"match": map[string]any{
			"context": "GATEWAY",
			"listener": map[string]any{
				"filterChain": map[string]any{
					"filter": map[string]any{
						"name": "envoy.filters.network.http_connection_manager",
						"subFilter": map[string]any{
							"name": anchor,
						},
					},
				},
			},
		},
		"patch": map[string]any{
			"operation": "INSERT_BEFORE",
			"value": map[string]any{
				"name": "envoy.filters.http.lua.maas-auth-denial",
				"typed_config": map[string]any{
					"@type": "type.googleapis.com/envoy.extensions.filters.http.lua.v3.Lua",
					"default_source_code": map[string]any{
						"inline_string": apiKeyDenialLua,
					},
				},
			},
		},
	}
}

func (r *TenantReconciler) ensureAuthDenialEnvoyFilter(
	ctx context.Context,
	log logr.Logger,
	tenant *maasv1alpha1.MaasTenantConfig,
	platformContext tenantreconcile.PlatformContext,
	mcfg *maasv1alpha1.Config,
) error {
	tenantID, err := tenantreconcile.TenantIdentifierFor(tenant)
	if err != nil {
		return err
	}
	gatewayName := platformContext.GatewayRef.Name
	ef := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "networking.istio.io/v1alpha3",
		"kind":       "EnvoyFilter",
		"spec": map[string]any{
			// Kuadrant inserts its RHCL wasm at priority 0. The IPP filter is
			// priority 10, so this Lua filter also precedes IPP on requests.
			"priority": int64(5),
			"workloadSelector": map[string]any{
				"labels": map[string]any{"gateway.networking.k8s.io/gateway-name": gatewayName},
			},
			"configPatches": []any{
				authDenialFilterPatch(fmt.Sprintf("extensions.istio.io/wasmplugin/%s.kuadrant-%s", r.GatewayNamespace, gatewayName)),
				authDenialFilterPatch("envoy.filters.http.wasm"),
			},
		},
	}}
	ef.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
	ef.SetName(tenantreconcile.AuthDenialEnvoyFilterName(tenantID))
	ef.SetNamespace(r.GatewayNamespace)
	applyUsageLogsEnvoyFilterMetadata(ef, tenant)
	if err := setUsageLogsEnvoyFilterConfigOwner(mcfg, ef, r.Scheme); err != nil {
		return fmt.Errorf("set Config owner on auth-denial EnvoyFilter %s: %w", ef.GetName(), err)
	}
	if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(ef), client.ForceOwnership, client.FieldOwner("maas-controller")); err != nil {
		return fmt.Errorf("apply auth-denial EnvoyFilter %s: %w", ef.GetName(), err)
	}
	log.V(1).Info("applied auth-denial EnvoyFilter", "name", ef.GetName(), "namespace", ef.GetNamespace())
	return nil
}
