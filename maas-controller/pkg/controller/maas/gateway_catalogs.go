package maas

import (
	"context"
	"fmt"
	"sort"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

// Preserve API/transport failures separately from invalid tenant configuration.
// A missing referenced object is unresolved configuration; other read failures
// must retry reconciliation rather than silently removing a contributor.
type gatewayCatalogReader struct {
	client.Reader
	readErr error
}

func (r *gatewayCatalogReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := r.Reader.Get(ctx, key, obj, opts...)
	if err != nil && !apierrors.IsNotFound(err) {
		r.readErr = err
	}
	return err
}

func (r *MaaSAuthPolicyReconciler) gatewayServingNamespace(namespace, tenantID, gatewayNamespace, gatewayName string) string {
	if r.TenantNamespace != "" && tenantID == "" && gatewayNamespace == r.GatewayNamespace && gatewayName == r.GatewayName {
		return r.TenantNamespace
	}
	return namespace
}

// The default API serves discovered legacy namespaces on its shared gateway.
// Dedicated tenants must never contribute catalogs to that gateway. Resolve a
// stable union from eligible policies/subscriptions, not the triggering object.
// The boolean reports unresolved exclusions, which require fail-closed check
// validation even when the remaining namespaces all have legacy/nil catalogs.
func (r *MaaSAuthPolicyReconciler) gatewayCatalogNamespaces(ctx context.Context, log logr.Logger, namespace, tenantID, gatewayNamespace, gatewayName string) ([]string, bool, error) {
	serving := r.gatewayServingNamespace(namespace, tenantID, gatewayNamespace, gatewayName)
	namespaces := map[string]bool{serving: true}
	if !r.TenantNamespaceDiscoveryEnabled || r.TenantNamespace == "" || serving != r.TenantNamespace || tenantID != "" || gatewayNamespace != r.GatewayNamespace || gatewayName != r.GatewayName {
		return []string{serving}, false, nil
	}
	var policies maasv1alpha1.MaaSAuthPolicyList
	if err := r.List(ctx, &policies); err != nil {
		return nil, false, fmt.Errorf("list gateway catalog policies: %w", err)
	}
	var subscriptions maasv1alpha1.MaaSSubscriptionList
	if err := r.List(ctx, &subscriptions); err != nil {
		return nil, false, fmt.Errorf("list gateway catalog subscriptions: %w", err)
	}
	candidates := map[string]bool{}
	for i := range policies.Items {
		if policies.Items[i].DeletionTimestamp.IsZero() {
			candidates[policies.Items[i].Namespace] = true
		}
	}
	for i := range subscriptions.Items {
		if subscriptions.Items[i].DeletionTimestamp.IsZero() {
			candidates[subscriptions.Items[i].Namespace] = true
		}
	}
	unresolved := false
	for candidate := range candidates {
		if candidate == serving {
			continue
		}
		allowed, err := tenantNamespaceAllowed(ctx, r.Client, candidate, r.TenantNamespace, true)
		if err != nil {
			return nil, false, err
		}
		if !allowed {
			continue
		}
		included, invalid, err := r.gatewayContributor(ctx, log, candidate, gatewayNamespace, gatewayName)
		if err != nil {
			return nil, false, err
		}
		unresolved = unresolved || invalid
		if included {
			namespaces[candidate] = true
		}
	}
	result := make([]string, 0, len(namespaces))
	for ns := range namespaces {
		result = append(result, ns)
	}
	sort.Strings(result)
	return result, unresolved, nil
}

func (r *MaaSAuthPolicyReconciler) gatewayContributor(ctx context.Context, log logr.Logger, namespace, gatewayNamespace, gatewayName string) (bool, bool, error) {
	reader := &gatewayCatalogReader{Reader: r.Client}
	tenant, err := fetchTenantForNamespace(ctx, reader, namespace)
	if reader.readErr != nil {
		return false, false, reader.readErr
	}
	if err == nil {
		var id string
		id, err = tenant.identifier()
		if err == nil && id != "" {
			return false, false, nil
		}
	}
	if err == nil {
		var platform tenantreconcile.PlatformContext
		platform, err = tenant.platformContext(ctx, reader, fallbackTenantGatewayRef(r.GatewayName, r.GatewayNamespace))
		if reader.readErr != nil {
			return false, false, reader.readErr
		}
		if err == nil {
			return platform.GatewayRef.Namespace == gatewayNamespace && platform.GatewayRef.Name == gatewayName, false, nil
		}
	}
	log.V(1).Info("excluding unresolved gateway catalog contributor", "namespace", namespace, "reason", err)
	return false, true, nil
}

func (r *MaaSAuthPolicyReconciler) gatewayGuardrailHeaders(ctx context.Context, namespaces []string) ([]guardrailHeader, error) {
	var result []guardrailHeader
	seen := map[string]bool{}
	for _, namespace := range namespaces {
		headers, err := r.guardrailHeaders(ctx, namespace)
		if err != nil {
			return nil, err
		}
		// A legacy nil catalog cannot erase another namespace's validated
		// catalog, including an explicitly empty fail-closed catalog.
		if headers != nil && result == nil {
			result = []guardrailHeader{}
		}
		for _, header := range headers {
			if !seen[header.ID] {
				seen[header.ID] = true
				result = append(result, header)
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (r *MaaSAuthPolicyReconciler) gatewaySubscriptionHeaderNames(ctx context.Context, namespaces []string) ([]string, error) {
	seen := map[string]bool{}
	for _, namespace := range namespaces {
		headers, err := r.subscriptionHeaderNames(ctx, namespace)
		if err != nil {
			return nil, err
		}
		for _, header := range headers {
			seen[header] = true
		}
	}
	result := make([]string, 0, len(seen))
	for header := range seen {
		result = append(result, header)
	}
	sort.Strings(result)
	return result, nil
}
