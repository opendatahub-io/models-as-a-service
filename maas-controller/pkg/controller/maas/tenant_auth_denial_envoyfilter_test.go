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

//nolint:testpackage
package maas

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"

	. "github.com/onsi/gomega"
)

func TestEnsureAuthDenialEnvoyFilter(t *testing.T) {
	for _, tc := range []struct {
		name, tenantNamespace, tenantName, tenantID, gatewayName string
		skipIPP                                                  bool
	}{
		{"default tenant with IPP", usageLogsTestDefaultTenant, tenantreconcile.DefaultAITenantName, "", tenantreconcile.DefaultAITenantName, false},
		{"secondary tenant with Praxis", "ai-tenant-redteam", "redteam", "redteam", "redteam-gateway", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			s := tenantTestScheme(t)
			cfg := usageLogsConfig(false)
			tenant := usageLogsTenantConfig(tc.tenantNamespace, tc.tenantName, usageLogsTestAITenantNS)
			// Filter deployment must not depend on telemetry or the IPP stack.
			tenant.Spec.Telemetry = nil
			cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cfg, tenant).Build()
			r := newUsageLogsReconciler(t, s, cl, nil)
			platform := defaultUsageLogsPlatformContext()
			platform.GatewayRef.Name = tc.gatewayName
			platform.SkipIPP = tc.skipIPP
			g.Expect(r.ensureAuthDenialEnvoyFilter(context.Background(), ctrl.Log, tenant, platform, cfg)).To(Succeed())

			ef := &unstructured.Unstructured{}
			ef.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
			g.Expect(cl.Get(context.Background(), client.ObjectKey{
				Name: tenantreconcile.AuthDenialEnvoyFilterName(tc.tenantID), Namespace: usageLogsTestGatewayNS,
			}, ef)).To(Succeed())
			expectUsageLogsEnvoyFilterOwnedByConfig(g, ef, cfg)
			priority, found, err := unstructured.NestedInt64(ef.Object, "spec", "priority")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(priority).To(BeNumerically(">", 0))
			g.Expect(priority).To(BeNumerically("<", tenantreconcile.PayloadProcessingEnvoyFilterPriority))
			selector, found, err := unstructured.NestedStringMap(ef.Object, "spec", "workloadSelector", "labels")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(selector["gateway.networking.k8s.io/gateway-name"]).To(Equal(tc.gatewayName))

			patches, found, err := unstructured.NestedSlice(ef.Object, "spec", "configPatches")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(patches).To(HaveLen(2))
			anchors := []string{
				"extensions.istio.io/wasmplugin/" + usageLogsTestGatewayNS + ".kuadrant-" + tc.gatewayName,
				"envoy.filters.http.wasm",
			}
			for i, patch := range patches {
				p := patch.(map[string]any)
				anchor, found, err := unstructured.NestedString(p, "match", "listener", "filterChain", "filter", "subFilter", "name")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(found).To(BeTrue())
				g.Expect(anchor).To(Equal(anchors[i]))
				operation, found, err := unstructured.NestedString(p, "patch", "operation")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(found).To(BeTrue())
				g.Expect(operation).To(Equal("INSERT_BEFORE"))
				lua, found, err := unstructured.NestedString(p, "patch", "value", "typed_config", "default_source_code", "inline_string")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(found).To(BeTrue())
				g.Expect(lua).To(Equal(apiKeyDenialLua))
			}
		})
	}
}

func TestAuthDenialLuaResponseMapping(t *testing.T) {
	// Keep the generated Lua paired with the policy marker values and the JSON
	// error contract. The CEL truth table is tested in TestAPIKeyDenialCEL.
	for _, part := range []string{
		`handle:headers():remove(marker)`,
		`headers:remove(marker)`,
		`headers:get(":status") ~= "403"`,
		`denial == "invalid"`, `status = "401"`, `error_type = "authentication_error"`,
		`denial == "unavailable"`, `status = "503"`, `error_type = "server_error"`,
		`handle:body(true):setBytes(body)`,
		`headers:replace(":status", status)`,
		`headers:replace("content-length", tostring(#body))`,
	} {
		if !strings.Contains(apiKeyDenialLua, part) {
			t.Errorf("Lua filter missing %q", part)
		}
	}
}

func TestTenantCleanupRemovesAuthDenialEnvoyFilter(t *testing.T) {
	g := NewWithT(t)
	s := tenantTestScheme(t)
	tenant := usageLogsTenantConfig("ai-tenant-redteam", "redteam", usageLogsTestAITenantNS)
	ef := &unstructured.Unstructured{}
	ef.SetGroupVersionKind(tenantreconcile.GVKEnvoyFilter)
	ef.SetName(tenantreconcile.AuthDenialEnvoyFilterName("redteam"))
	ef.SetNamespace(usageLogsTestGatewayNS)
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(tenant, ef).Build()
	r := newUsageLogsReconciler(t, s, cl, func(r *TenantReconciler) { r.AppNamespace = "opendatahub" })
	g.Expect(r.cleanupTenantResources(context.Background(), ctrl.Log, tenant)).To(Succeed())
	err := cl.Get(context.Background(), client.ObjectKeyFromObject(ef), ef)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}
