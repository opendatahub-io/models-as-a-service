package maas

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

const ogxAPIAuthPartOf = "ogx-api-auth"

func ogxAPINames(tenant string) (route, policy string) {
	if tenant == "models-as-a-service" {
		return "ogx-api", "ogx-api-auth"
	}
	return "ogx-api-" + tenant, "ogx-api-auth-" + tenant
}

func (r *AITenantReconciler) ensureOGXAPIAuthPolicy(ctx context.Context, tenant *maasv1alpha1.AITenant, tenantNamespace string, gateway maasv1alpha1.TenantGatewayRef) error {
	routeName, policyName := ogxAPINames(tenant.Name)
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	key := client.ObjectKey{Name: policyName, Namespace: tenantNamespace}
	if tenant.Spec.AgenticBackendRef == "" {
		// Do not remove authorization while a previously served route still exists.
		route := &unstructured.Unstructured{}
		route.SetGroupVersionKind(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"})
		if err := r.Get(ctx, client.ObjectKey{Name: routeName, Namespace: tenantNamespace}, route); err == nil {
			return nil
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to check OGX route before auth cleanup: %w", err)
		}
		if err := r.Get(ctx, key, policy); apierrors.IsNotFound(err) {
			return nil
		} else if err != nil {
			return fmt.Errorf("failed to get OGX AuthPolicy for cleanup: %w", err)
		}
		if policy.GetLabels()["app.kubernetes.io/part-of"] != ogxAPIAuthPartOf {
			return fmt.Errorf("OGX AuthPolicy %s/%s is not MaaS-owned", tenantNamespace, policyName)
		}
		return r.Delete(ctx, policy)
	}

	// Start from MaaS's current gateway auth contract so the validation callback,
	// service CA, and accepted key format stay aligned with the installed MaaS.
	base := &unstructured.Unstructured{}
	base.SetGroupVersionKind(policy.GroupVersionKind())
	base.SetNamespace(gateway.Namespace)
	base.SetName((&MaaSAuthPolicyReconciler{GatewayName: r.GatewayName, GatewayNamespace: r.GatewayNamespace}).gatewayAuthPolicyName(gateway.Namespace, gateway.Name))
	if err := r.Get(ctx, client.ObjectKeyFromObject(base), base); err != nil {
		return fmt.Errorf("failed to get MaaS Gateway AuthPolicy: %w", err)
	}
	if base.GetLabels()["app.kubernetes.io/managed-by"] != "maas-controller" {
		return fmt.Errorf("gateway AuthPolicy %s/%s is not MaaS-owned", base.GetNamespace(), base.GetName())
	}
	spec, err := ogxAPIAuthSpec(base, tenant, tenantNamespace, routeName)
	if err != nil {
		return err
	}
	policy.SetName(policyName)
	policy.SetNamespace(tenantNamespace)
	policy.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "maas-controller", "app.kubernetes.io/part-of": ogxAPIAuthPartOf})
	if err := unstructured.SetNestedMap(policy.Object, spec, "spec"); err != nil {
		return fmt.Errorf("failed to set OGX AuthPolicy spec: %w", err)
	}
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(policy.GroupVersionKind())
	if err := r.Get(ctx, key, existing); apierrors.IsNotFound(err) {
		return r.Create(ctx, policy)
	} else if err != nil {
		return fmt.Errorf("failed to get OGX AuthPolicy: %w", err)
	}
	if existing.GetLabels()["app.kubernetes.io/part-of"] != ogxAPIAuthPartOf {
		return fmt.Errorf("OGX AuthPolicy %s/%s is not MaaS-owned", tenantNamespace, policyName)
	}
	current, _, _ := unstructured.NestedMap(existing.Object, "spec")
	if specMatchesDesired(spec, current) {
		return nil
	}
	if err := unstructured.SetNestedMap(existing.Object, spec, "spec"); err != nil {
		return fmt.Errorf("failed to update OGX AuthPolicy spec: %w", err)
	}
	return r.Update(ctx, existing)
}

func ogxAPIAuthSpec(base *unstructured.Unstructured, tenant *maasv1alpha1.AITenant, namespace, route string) (map[string]any, error) {
	spec, found, err := unstructured.NestedMap(base.Object, "spec")
	if err != nil || !found {
		return nil, fmt.Errorf("failed to read MaaS gateway AuthPolicy spec: %v", err)
	}
	spec["targetRef"] = map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": route}
	rules := spec["defaults"].(map[string]any)["rules"].(map[string]any)
	metadata := rules["metadata"].(map[string]any)
	delete(metadata, "subscription-info")
	validation := metadata["apiKeyValidation"].(map[string]any)
	httpRule := validation["http"].(map[string]any)
	validationURL, err := url.Parse(httpRule["url"].(string))
	if err != nil || !strings.HasPrefix(validationURL.Hostname(), "maas-api.") && !strings.HasPrefix(validationURL.Hostname(), "maas-api-") {
		return nil, fmt.Errorf("invalid MaaS API-key validation endpoint")
	}
	service := "maas-api"
	tenantID := tenant.Name
	if tenantID != "models-as-a-service" {
		service += "-" + tenantID
	}
	parts := strings.SplitN(validationURL.Host, ".", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid MaaS API-key validation Service")
	}
	validationURL.Host = service + "." + parts[1]
	httpRule["url"] = validationURL.String()
	authentication := rules["authentication"].(map[string]any)
	for name := range authentication {
		if name != "api-keys" && name != "api-keys-x-api-key" {
			delete(authentication, name)
		}
	}
	authorization := rules["authorization"].(map[string]any)
	for name := range authorization {
		if name != "auth-valid" && name != "deny-client-identity-headers" {
			delete(authorization, name)
		}
	}
	authValid := authorization["auth-valid"].(map[string]any)
	delete(authValid, "cache")
	authValid["opa"].(map[string]any)["rego"] = fmt.Sprintf(`allow {
  input.auth.metadata.apiKeyValidation.valid == true
  input.auth.metadata.apiKeyValidation.tenant == %q
  input.auth.metadata.apiKeyValidation.subscription != ""
  input.auth.metadata.apiKeyValidation.username != ""
}`, tenantID)
	path := "/" + namespace + "/v1/"
	authorization["deny-direct-ogx-path"] = map[string]any{
		"when":            []any{map[string]any{"predicate": fmt.Sprintf(`!request.path.startsWith(%q)`, path)}},
		"patternMatching": map[string]any{"patterns": []any{map[string]any{"predicate": "false"}}},
	}
	patterns := authorization["deny-client-identity-headers"].(map[string]any)["patternMatching"].(map[string]any)["patterns"].([]any)
	for _, header := range []string{"x-maas-subscription", "x-maas-owner-tenant", "x-ogx-route-tenant", "x-tenant-id", "x-user-id"} {
		patterns = append(patterns, map[string]any{"predicate": fmt.Sprintf(`!(%q in request.headers)`, header)})
	}
	authorization["deny-client-identity-headers"].(map[string]any)["patternMatching"].(map[string]any)["patterns"] = patterns
	headers := rules["response"].(map[string]any)["success"].(map[string]any)["headers"].(map[string]any)
	for name := range headers {
		if name != "X-MaaS-Username" && name != "X-MaaS-Subscription" && name != "X-MaaS-Owner-Tenant" {
			delete(headers, name)
		}
	}
	for _, name := range []string{"X-MaaS-Username", "X-MaaS-Subscription", "X-MaaS-Owner-Tenant"} {
		delete(headers[name].(map[string]any), "when")
	}
	headers["X-MaaS-Username"].(map[string]any)["plain"] = map[string]any{"expression": "auth.metadata.apiKeyValidation.username"}
	headers["X-MaaS-Subscription"].(map[string]any)["plain"] = map[string]any{"expression": "auth.metadata.apiKeyValidation.subscription"}
	headers["X-MaaS-Owner-Tenant"].(map[string]any)["plain"] = map[string]any{"expression": `auth.metadata.apiKeyValidation.tenant + "_" + auth.metadata.apiKeyValidation.subscription`}
	return spec, nil
}
