package tenantreconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// PostRender mutates rendered resources the same way as ODH modelsasservice post-kustomize actions.
func PostRender(ctx context.Context, log logr.Logger, tenant *maasv1alpha1.Tenant, resources []unstructured.Unstructured) ([]unstructured.Unstructured, error) {
	gatewayNamespace := tenant.Spec.GatewayRef.Namespace
	gatewayName := tenant.Spec.GatewayRef.Name

	// Filter out resources with opendatahub.io/managed: false annotation
	var filteredResources []unstructured.Unstructured
	for i := range resources {
		resource := &resources[i]

		// Skip resources with opendatahub.io/managed: false annotation
		annotations := resource.GetAnnotations()
		if annotations != nil && annotations["opendatahub.io/managed"] == "false" {
			log.V(2).Info("Skipping resource due to opendatahub.io/managed=false annotation",
				"kind", resource.GetKind(), "name", resource.GetName(), "namespace", resource.GetNamespace())
			continue
		}

		gvk := resource.GroupVersionKind()
		switch {
		case gvk == GVKAuthPolicy && resource.GetName() == GatewayDefaultAuthPolicyName:
			if err := configureAuthPolicy(log, resource, gatewayNamespace, gatewayName); err != nil {
				return nil, err
			}
		case gvk == GVKTokenRateLimitPolicy && resource.GetName() == GatewayTokenRateLimitDefaultDenyPolicyName:
			if err := configureTokenRateLimitPolicy(log, resource, gatewayNamespace, gatewayName); err != nil {
				return nil, err
			}
		case gvk == GVKDestinationRule && resource.GetName() == GatewayDestinationRuleName:
			configureDestinationRule(log, resource, gatewayNamespace)
		}

		filteredResources = append(filteredResources, *resource)
	}

	setManagedFalseAnnotation(filteredResources)

	if err := configureExternalOIDC(log, tenant, filteredResources); err != nil {
		return nil, err
	}
	if err := configureTelemetryPolicyResources(log, tenant, &filteredResources); err != nil {
		return nil, err
	}
	if err := configureIstioTelemetryResources(log, tenant, &filteredResources); err != nil {
		return nil, err
	}
	if err := configureConfigHashAnnotation(log, filteredResources); err != nil {
		return nil, err
	}
	_ = ctx
	return filteredResources, nil
}

func configureAuthPolicy(log logr.Logger, resource *unstructured.Unstructured, gatewayNamespace, gatewayName string) error {
	log.V(4).Info("Configuring AuthPolicy", "name", resource.GetName(), "newNamespace", gatewayNamespace, "newTargetGateway", gatewayName)
	resource.SetNamespace(gatewayNamespace)
	if err := unstructured.SetNestedField(resource.Object, gatewayName, "spec", "targetRef", "name"); err != nil {
		return fmt.Errorf("failed to set spec.targetRef.name on AuthPolicy: %w", err)
	}
	return nil
}

func configureTokenRateLimitPolicy(log logr.Logger, resource *unstructured.Unstructured, gatewayNamespace, gatewayName string) error {
	log.V(4).Info("Configuring TokenRateLimitPolicy", "name", resource.GetName(), "newNamespace", gatewayNamespace, "newTargetGateway", gatewayName)
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

// setManagedFalseAnnotation marks the maas-api AuthPolicy with opendatahub.io/managed=false
// so the ODH operator does not reconcile it back to its defaults after the Tenant reconciler
// has applied OIDC, audience, and other customizations.
func setManagedFalseAnnotation(resources []unstructured.Unstructured) {
	for i := range resources {
		r := &resources[i]
		if r.GroupVersionKind() == GVKAuthPolicy && r.GetName() == MaaSAPIAuthPolicyName {
			ann := r.GetAnnotations()
			if ann == nil {
				ann = make(map[string]string)
			}
			ann["opendatahub.io/managed"] = "false"
			r.SetAnnotations(ann)
			return
		}
	}
}

func configureExternalOIDC(log logr.Logger, tenant *maasv1alpha1.Tenant, resources []unstructured.Unstructured) error {
	if tenant.Spec.ExternalOIDC == nil {
		return nil
	}
	oidc := tenant.Spec.ExternalOIDC
	for i := range resources {
		resource := &resources[i]
		if resource.GroupVersionKind() == GVKAuthPolicy && resource.GetName() == MaaSAPIAuthPolicyName {
			return patchAuthPolicyWithOIDC(log, resource, oidc)
		}
	}
	return fmt.Errorf("rendered resources are missing AuthPolicy %q while spec.externalOIDC is configured — refusing to deploy without OIDC rules", MaaSAPIAuthPolicyName)
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
		`(size(auth.identity.groups) > 0 ? ` +
		`'["system:authenticated","' + auth.identity.groups.join('","') + '"]' : ` +
		`'["system:authenticated"]') : ` +
		`'["' + auth.identity.user.groups.join('","') + '"]'`
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

func configureTelemetryPolicyResources(log logr.Logger, tenant *maasv1alpha1.Tenant, resources *[]unstructured.Unstructured) error {
	if !isTelemetryEnabled(tenant.Spec.Telemetry) {
		return nil
	}
	// Caller should have checked CRD; still skip if API missing at apply time.
	gatewayNamespace := tenant.Spec.GatewayRef.Namespace
	gatewayName := tenant.Spec.GatewayRef.Name
	metricLabels := buildTelemetryLabels(log, tenant.Spec.Telemetry)
	tp := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "extensions.kuadrant.io/v1alpha1",
			"kind":       "TelemetryPolicy",
			"metadata": map[string]any{
				"name":      TelemetryPolicyName,
				"namespace": gatewayNamespace,
				"labels": map[string]any{
					"app.kubernetes.io/part-of": "maas-observability",
					LabelTenantName:             tenant.Name,
					LabelTenantNamespace:        tenant.Namespace,
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
	log.V(2).Info("Appending TelemetryPolicy", "name", TelemetryPolicyName, "namespace", gatewayNamespace)
	*resources = append(*resources, *tp)
	return nil
}

func configureIstioTelemetryResources(log logr.Logger, tenant *maasv1alpha1.Tenant, resources *[]unstructured.Unstructured) error {
	if !isTelemetryEnabled(tenant.Spec.Telemetry) {
		return nil
	}
	gatewayNamespace := tenant.Spec.GatewayRef.Namespace
	gatewayName := tenant.Spec.GatewayRef.Name
	istioTelemetry := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "telemetry.istio.io/v1",
			"kind":       "Telemetry",
			"metadata": map[string]any{
				"name":      IstioTelemetryName,
				"namespace": gatewayNamespace,
				"labels": map[string]any{
					"app.kubernetes.io/part-of": "maas-observability",
					LabelTenantName:             tenant.Name,
					LabelTenantNamespace:        tenant.Namespace,
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
	log.V(2).Info("Appending Istio Telemetry", "name", IstioTelemetryName, "namespace", gatewayNamespace)
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

func configureConfigHashAnnotation(log logr.Logger, resources []unstructured.Unstructured) error {
	var configMap *corev1.ConfigMap
	for idx := range resources {
		resource := &resources[idx]
		if resource.GroupVersionKind() == GVKConfigMap && resource.GetName() == MaaSParametersConfigMapName {
			cm := &corev1.ConfigMap{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(resource.Object, cm); err != nil {
				return fmt.Errorf("failed to convert ConfigMap: %w", err)
			}
			configMap = cm
			break
		}
	}
	if configMap == nil {
		log.V(1).Info("ConfigMap not found in rendered resources, skipping config hash annotation", "expectedName", MaaSParametersConfigMapName)
		return nil
	}

	configHash := hashConfigMapData(configMap.Data)
	log.V(4).Info("Computed ConfigMap hash", "hash", configHash, "configMap", configMap.Name)

	var deployment *appsv1.Deployment
	depIdx := -1
	for idx := range resources {
		resource := &resources[idx]
		if resource.GroupVersionKind() == GVKDeployment && resource.GetName() == MaaSAPIDeploymentName {
			dep := &appsv1.Deployment{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(resource.Object, dep); err != nil {
				return fmt.Errorf("failed to convert Deployment: %w", err)
			}
			deployment = dep
			depIdx = idx
			break
		}
	}
	if deployment == nil {
		log.V(1).Info("Deployment not found in rendered resources, skipping config hash annotation", "expectedName", MaaSAPIDeploymentName)
		return nil
	}

	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = make(map[string]string)
	}
	annotationKey := LabelODHAppPrefix + "/maas-config-hash"
	deployment.Spec.Template.Annotations[annotationKey] = configHash

	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(deployment)
	if err != nil {
		return fmt.Errorf("failed to convert Deployment back to unstructured: %w", err)
	}
	resources[depIdx].Object = u

	return nil
}

func hashConfigMapData(data map[string]string) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(data[k])
		sb.WriteString("\n")
	}
	hash := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(hash[:])
}

// CustomizeParams writes gateway/app-namespace/cluster-audience and optional API key days into overlay params.env
// (same keys as ODH customizeManifests; images use RELATED_IMAGE_* like ODH Init + ApplyParams).
func CustomizeParams(manifestDir string, tenant *maasv1alpha1.Tenant, appNamespace string, clusterAudience string) error {
	params := map[string]string{
		"gateway-namespace": tenant.Spec.GatewayRef.Namespace,
		"gateway-name":      tenant.Spec.GatewayRef.Name,
		"app-namespace":     appNamespace,
	}
	if tenant.Spec.APIKeys != nil && tenant.Spec.APIKeys.MaxExpirationDays != nil {
		params["api-key-max-expiration-days"] = strconv.FormatInt(int64(*tenant.Spec.APIKeys.MaxExpirationDays), 10)
	}
	if clusterAudience != "" {
		params["cluster-audience"] = clusterAudience
	}
	return ApplyParams(manifestDir, "params.env", ImageParamKeys, params)
}
