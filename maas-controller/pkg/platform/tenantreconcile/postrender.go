package tenantreconcile

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// PostRender mutates rendered resources after kustomize build. It patches all
// dynamic values (images, gateway config, namespace, audience, env vars) and
// applies OIDC, telemetry, and managed-annotation customizations.
func PostRender(ctx context.Context, log logr.Logger, tenant client.Object, resources []unstructured.Unstructured, params PlatformParams) ([]unstructured.Unstructured, error) {
	gatewayNamespace := params.GatewayNamespace
	gatewayName := params.GatewayName
	tenantID := params.TenantIdentifier

	var filteredResources []unstructured.Unstructured
	for i := range resources {
		resource := &resources[i]

		annotations := resource.GetAnnotations()
		if annotations != nil && annotations[AnnotationManaged] == "false" {
			log.V(2).Info("Skipping resource due to opendatahub.io/managed=false annotation",
				"kind", resource.GetKind(), "name", resource.GetName(), "namespace", resource.GetNamespace())
			continue
		}

		gvk := resource.GroupVersionKind()
		switch {
		case gvk == GVKTokenRateLimitPolicy && resource.GetName() == baseGatewayTokenRateLimitDefaultDenyPolicyName:
			if err := configureTokenRateLimitPolicy(log, resource, gatewayNamespace, gatewayName, tenantID); err != nil {
				return nil, err
			}
		case gvk == GVKDestinationRule && resource.GetName() == baseGatewayDestinationRuleName:
			configureDestinationRule(log, resource, gatewayNamespace)
		case gvk.Group == "" && gvk.Kind == "Service" && resource.GetName() == "maas-api":
			// Make TLS secret name unique per tenant
			if err := configureMaaSAPIService(log, resource, tenantID); err != nil {
				return nil, err
			}
		case gvk.Group == "apps" && gvk.Kind == "Deployment" && resource.GetName() == "maas-api":
			// Update Deployment to mount tenant-specific TLS secret
			if err := configureMaaSAPIDeployment(log, resource, tenantID); err != nil {
				return nil, err
			}
		case gvk == GVKHTTPRoute && resource.GetName() == "maas-api-route":
			// Configure per-tenant HTTPRoute
			if err := configureMaaSAPIHTTPRoute(log, resource, gatewayNamespace, gatewayName, params); err != nil {
				return nil, err
			}
		}

		filteredResources = append(filteredResources, *resource)
	}

	if err := configureExternalOIDC(log, params); err != nil {
		return nil, err
	}
	if err := configureTelemetryPolicyResources(log, tenant, &filteredResources, params); err != nil {
		return nil, err
	}
	if err := configureIstioTelemetryResources(log, tenant, &filteredResources, params); err != nil {
		return nil, err
	}
	if err := applyPlatformParams(log, filteredResources, params); err != nil {
		return nil, err
	}
	_ = ctx
	return filteredResources, nil
}

func configureTokenRateLimitPolicy(log logr.Logger, resource *unstructured.Unstructured, gatewayNamespace, gatewayName, tenantID string) error {
	// Generate unique per-tenant name to avoid conflicts when multiple tenants share the same gateway namespace
	newName := GatewayTokenRateLimitDefaultDenyPolicyName(tenantID)
	log.V(4).Info("Configuring TokenRateLimitPolicy",
		"oldName", resource.GetName(),
		"newName", newName,
		"namespace", gatewayNamespace,
		"targetGateway", gatewayName)

	resource.SetName(newName)
	resource.SetNamespace(gatewayNamespace)
	if err := unstructured.SetNestedField(resource.Object, gatewayName, "spec", "targetRef", "name"); err != nil {
		return fmt.Errorf("failed to set spec.targetRef.name on TokenRateLimitPolicy: %w", err)
	}
	return nil
}

func configureDestinationRule(log logr.Logger, resource *unstructured.Unstructured, gatewayNamespace string) {
	log.V(4).Info("Configuring DestinationRule", "name", resource.GetName(), "newNamespace", gatewayNamespace)
	resource.SetNamespace(gatewayNamespace)
}

func configureMaaSAPIService(log logr.Logger, resource *unstructured.Unstructured, tenantID string) error {
	// For default tenant (tenantID=""), use "maas-api-serving-cert"
	// For other tenants, use "maas-api-{tenantID}-serving-cert"
	secretName := "maas-api-serving-cert"
	if tenantID != "" {
		secretName = fmt.Sprintf("maas-api-%s-serving-cert", tenantID)
	}

	annotations := resource.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations["service.beta.openshift.io/serving-cert-secret-name"] = secretName
	resource.SetAnnotations(annotations)

	log.V(4).Info("Configured maas-api Service TLS secret", "tenantID", tenantID, "secretName", secretName)
	return nil
}

func configureMaaSAPIDeployment(log logr.Logger, resource *unstructured.Unstructured, tenantID string) error {
	// Update the Deployment to mount the correct tenant-specific TLS secret
	secretName := "maas-api-serving-cert"
	if tenantID != "" {
		secretName = fmt.Sprintf("maas-api-%s-serving-cert", tenantID)
	}

	// Navigate to spec.template.spec.volumes and find the tls-cert volume
	volumes, found, err := unstructured.NestedSlice(resource.Object, "spec", "template", "spec", "volumes")
	if err != nil {
		return fmt.Errorf("failed to get volumes: %w", err)
	}
	if !found {
		return errors.New("no volumes found in deployment")
	}

	// Find and update the maas-api-tls volume's secret name
	for i, vol := range volumes {
		volMap, ok := vol.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(volMap, "name")
		if name == "maas-api-tls" {
			if err := unstructured.SetNestedField(volMap, secretName, "secret", "secretName"); err != nil {
				return fmt.Errorf("failed to set maas-api-tls secret name: %w", err)
			}
			volumes[i] = volMap
			break
		}
	}

	if err := unstructured.SetNestedSlice(resource.Object, volumes, "spec", "template", "spec", "volumes"); err != nil {
		return fmt.Errorf("failed to set volumes: %w", err)
	}

	log.V(4).Info("Configured maas-api Deployment TLS secret volume", "tenantID", tenantID, "secretName", secretName)
	return nil
}

func configureExternalOIDC(log logr.Logger, params PlatformParams) error {
	if params.ExternalOIDC == nil {
		return nil
	}
	// OIDC is configured in the singleton maas-gateway-auth AuthPolicy managed by
	// maas-controller (see MaaSAuthPolicyReconciler.buildGatewayAuthPolicySpec).
	// The route-level maas-api-auth-policy has been removed, so there is nothing
	// to patch in the kustomize-rendered resources here.
	log.V(1).Info("external OIDC configured via gateway-level AuthPolicy; no kustomize resources to patch")
	return nil
}

func patchAuthPolicyWithOIDC(log logr.Logger, resource *unstructured.Unstructured, oidc *maasv1alpha1.TenantExternalOIDCConfig) error {
	ttl := int64(oidc.TTL)
	if ttl == 0 {
		ttl = 300
	}
	if err := unstructured.SetNestedField(resource.Object, map[string]any{
		"when": []any{
			map[string]any{
				"predicate": `!request.headers.authorization.startsWith("Bearer sk-oai-") && request.headers.authorization.matches("^Bearer [^.]+\\.[^.]+\\.[^.]+$")`,
			},
		},
		"jwt": map[string]any{
			"issuerUrl": oidc.IssuerURL,
			"ttl":       ttl,
		},
		"priority": int64(1),
	}, "spec", "rules", "authentication", "oidc-identities"); err != nil {
		return fmt.Errorf("failed to set oidc-identities: %w", err)
	}
	if err := unstructured.SetNestedField(resource.Object, int64(2),
		"spec", "rules", "authentication", "openshift-identities", "priority"); err != nil {
		return fmt.Errorf("failed to set openshift-identities priority: %w", err)
	}
	if err := unstructured.SetNestedField(resource.Object, []any{
		map[string]any{
			"predicate": `!request.headers.authorization.startsWith("Bearer sk-oai-")`,
		},
	}, "spec", "rules", "authentication", "openshift-identities", "when"); err != nil {
		return fmt.Errorf("failed to set openshift-identities when: %w", err)
	}
	if err := unstructured.SetNestedField(resource.Object, map[string]any{
		"when": []any{
			map[string]any{
				"predicate": `!request.headers.authorization.startsWith("Bearer sk-oai-") && request.headers.authorization.matches("^Bearer [^.]+\\.[^.]+\\.[^.]+$")`,
			},
		},
		"patternMatching": map[string]any{
			"patterns": []any{
				map[string]any{
					"selector": "auth.identity.azp",
					"operator": "eq",
					"value":    oidc.ClientID,
				},
			},
		},
		"priority": int64(1),
	}, "spec", "rules", "authorization", "oidc-client-bound"); err != nil {
		return fmt.Errorf("failed to set oidc-client-bound: %w", err)
	}
	if err := unstructured.SetNestedField(resource.Object, map[string]any{
		"expression": `has(auth.identity.preferred_username) ? auth.identity.preferred_username : (has(auth.identity.sub) ? auth.identity.sub : auth.identity.user.username)`,
	}, "spec", "rules", "response", "success", "headers", "X-MaaS-Username-OC", "plain"); err != nil {
		return fmt.Errorf("failed to set X-MaaS-Username-OC: %w", err)
	}
	groupsExpr := `has(auth.identity.groups) ? ` +
		`(size(auth.identity.groups) > 0 && auth.identity.groups.all(g, g.matches('^[A-Za-z0-9:._/-]+$')) ? ` +
		`'["system:authenticated","' + auth.identity.groups.join('","') + '"]' : ` +
		`'["system:authenticated"]') : ` +
		`(has(auth.identity.user.groups) && size(auth.identity.user.groups) > 0 ? ` +
		`'["system:authenticated","' + auth.identity.user.groups.join('","') + '"]' : ` +
		`'["system:authenticated"]')`
	if err := unstructured.SetNestedField(resource.Object, map[string]any{
		"expression": groupsExpr,
	}, "spec", "rules", "response", "success", "headers", "X-MaaS-Group-OC", "plain"); err != nil {
		return fmt.Errorf("failed to set X-MaaS-Group-OC: %w", err)
	}
	log.Info("Patched maas-api AuthPolicy with external OIDC configuration", "issuerUrl", oidc.IssuerURL, "clientId", oidc.ClientID)
	return nil
}

func isTelemetryEnabled(t *maasv1alpha1.TenantTelemetryConfig) bool {
	if t == nil {
		return false
	}
	if t.Enabled == nil {
		return false
	}
	return *t.Enabled
}

func configureTelemetryPolicyResources(log logr.Logger, tenant client.Object, resources *[]unstructured.Unstructured, params PlatformParams) error {
	telemetry := telemetryConfigFor(tenant)
	if !isTelemetryEnabled(telemetry) {
		return nil
	}
	// Caller should have checked CRD; still skip if API missing at apply time.
	gatewayNamespace := params.GatewayNamespace
	gatewayName := params.GatewayName
	tenantID := params.TenantIdentifier
	metricLabels := buildTelemetryLabels(log, telemetry)
	tp := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "extensions.kuadrant.io/v1alpha1",
			"kind":       "TelemetryPolicy",
			"metadata": map[string]any{
				"name":      TelemetryPolicyName(tenantID),
				"namespace": gatewayNamespace,
				"labels": map[string]any{
					"app.kubernetes.io/part-of": "maas-observability",
					LabelTenantName:             tenantTrackingName(tenant),
					LabelTenantNamespace:        tenant.GetNamespace(),
				},
			},
			"spec": map[string]any{
				"targetRef": map[string]any{
					"group": "gateway.networking.k8s.io",
					"kind":  "Gateway",
					"name":  gatewayName,
				},
				"metrics": map[string]any{
					"default": map[string]any{
						"labels": metricLabels,
					},
				},
			},
		},
	}
	telemetryPolicyName := TelemetryPolicyName(tenantID)
	log.V(2).Info("Appending TelemetryPolicy", "name", telemetryPolicyName, "namespace", gatewayNamespace)
	*resources = append(*resources, *tp)
	return nil
}

func configureIstioTelemetryResources(log logr.Logger, tenant client.Object, resources *[]unstructured.Unstructured, params PlatformParams) error {
	if !isTelemetryEnabled(telemetryConfigFor(tenant)) {
		return nil
	}
	gatewayNamespace := params.GatewayNamespace
	gatewayName := params.GatewayName
	tenantID := params.TenantIdentifier
	istioTelemetry := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "telemetry.istio.io/v1",
			"kind":       "Telemetry",
			"metadata": map[string]any{
				"name":      IstioTelemetryName(tenantID),
				"namespace": gatewayNamespace,
				"labels": map[string]any{
					"app.kubernetes.io/part-of": "maas-observability",
					LabelTenantName:             tenantTrackingName(tenant),
					LabelTenantNamespace:        tenant.GetNamespace(),
				},
			},
			"spec": map[string]any{
				"selector": map[string]any{
					"matchLabels": map[string]any{
						"gateway.networking.k8s.io/gateway-name": gatewayName,
					},
				},
				"metrics": []any{
					map[string]any{
						"providers": []any{map[string]any{"name": "prometheus"}},
						"overrides": []any{
							map[string]any{
								"match": map[string]any{"metric": "REQUEST_DURATION", "mode": "CLIENT_AND_SERVER"},
								"tagOverrides": map[string]any{
									"subscription": map[string]any{
										"operation": "UPSERT",
										"value":     `request.headers["x-maas-subscription"]`,
									},
								},
							},
						},
					},
				},
			},
		},
	}
	istioTelemetryName := IstioTelemetryName(tenantID)
	log.V(2).Info("Appending Istio Telemetry", "name", istioTelemetryName, "namespace", gatewayNamespace)
	*resources = append(*resources, *istioTelemetry)
	return nil
}

func buildTelemetryLabels(log logr.Logger, config *maasv1alpha1.TenantTelemetryConfig) map[string]any {
	captureOrganization := true
	captureUser := false
	captureGroup := false
	captureModelUsage := true
	if config != nil && config.Metrics != nil {
		metrics := config.Metrics
		if metrics.CaptureOrganization != nil {
			captureOrganization = *metrics.CaptureOrganization
		}
		if metrics.CaptureUser != nil {
			captureUser = *metrics.CaptureUser
		}
		if metrics.CaptureGroup != nil {
			captureGroup = *metrics.CaptureGroup
		}
		if metrics.CaptureModelUsage != nil {
			captureModelUsage = *metrics.CaptureModelUsage
		}
	}
	labels := map[string]any{
		"subscription": "auth.identity.selected_subscription",
		"cost_center":  "auth.identity.subscription_info.costCenter",
	}
	if captureOrganization {
		labels["organization_id"] = "auth.identity.subscription_info.organizationId"
	}
	if captureUser {
		log.Info("WARNING: User identity metrics enabled - ensure GDPR/privacy compliance", "field", "captureUser", "value", true)
		labels["user"] = "auth.identity.userid"
	}
	if captureGroup {
		labels["group"] = "auth.identity.group"
	}
	if captureModelUsage {
		labels["model"] = "responseBodyJSON(\"/model\")"
	}
	return labels
}

func configureMaaSAPIHTTPRoute(log logr.Logger, resource *unstructured.Unstructured, gatewayNamespace, gatewayName string, params PlatformParams) error {
	tenantID := params.TenantIdentifier

	// Rename HTTPRoute for non-default tenants
	if tenantID != "" {
		newName := MaaSAPIRouteName(tenantID)
		log.V(4).Info("Renaming maas-api HTTPRoute", "oldName", resource.GetName(), "newName", newName)
		resource.SetName(newName)
	}

	// Get parentRefs array first
	parentRefs, found, err := unstructured.NestedSlice(resource.Object, "spec", "parentRefs")
	if err != nil || !found || len(parentRefs) == 0 {
		return fmt.Errorf("HTTPRoute has no parentRefs: %w", err)
	}

	// Update first parentRef
	parentRefMap, ok := parentRefs[0].(map[string]any)
	if !ok {
		return errors.New("parentRefs[0] is not a map")
	}
	parentRefMap["name"] = gatewayName
	parentRefMap["namespace"] = gatewayNamespace
	parentRefs[0] = parentRefMap

	if err := unstructured.SetNestedSlice(resource.Object, parentRefs, "spec", "parentRefs"); err != nil {
		return fmt.Errorf("failed to set HTTPRoute parentRefs: %w", err)
	}

	// Update backendRefs to point to tenant-specific maas-api service
	serviceName := MaaSAPIServiceName(tenantID)

	// Update both rules (v1/models and /maas-api)
	rules, found, err := unstructured.NestedSlice(resource.Object, "spec", "rules")
	if err != nil {
		return fmt.Errorf("failed to get HTTPRoute rules: %w", err)
	}
	if !found || len(rules) == 0 {
		return errors.New("HTTPRoute has no rules")
	}

	for i := range rules {
		ruleMap, ok := rules[i].(map[string]any)
		if !ok {
			continue
		}
		backendRefs, found, err := unstructured.NestedSlice(ruleMap, "backendRefs")
		if err != nil || !found || len(backendRefs) == 0 {
			continue
		}
		for j := range backendRefs {
			backendMap, ok := backendRefs[j].(map[string]any)
			if !ok {
				continue
			}
			if err := unstructured.SetNestedField(backendMap, serviceName, "name"); err != nil {
				return fmt.Errorf("failed to set backendRefs[%d].name: %w", j, err)
			}
			backendRefs[j] = backendMap
		}
		if err := unstructured.SetNestedSlice(ruleMap, backendRefs, "backendRefs"); err != nil {
			return fmt.Errorf("failed to set rule[%d].backendRefs: %w", i, err)
		}
		rules[i] = ruleMap
	}

	if err := unstructured.SetNestedSlice(resource.Object, rules, "spec", "rules"); err != nil {
		return fmt.Errorf("failed to set HTTPRoute rules: %w", err)
	}

	log.V(4).Info("Configured maas-api HTTPRoute",
		"tenantID", tenantID,
		"gateway", fmt.Sprintf("%s/%s", gatewayNamespace, gatewayName),
		"service", serviceName)
	return nil
}
