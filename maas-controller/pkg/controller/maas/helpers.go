package maas

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

// deletionTimestampSet returns true when an object's DeletionTimestamp transitions
// from nil to non-nil, indicating the object is being deleted. Use with
// predicate.Funcs{UpdateFunc: deletionTimestampSet} alongside
// GenerationChangedPredicate so that finalizer-based deletion handlers run.
func deletionTimestampSet(e event.UpdateEvent) bool {
	return e.ObjectOld.GetDeletionTimestamp().IsZero() &&
		!e.ObjectNew.GetDeletionTimestamp().IsZero()
}

// validateCELValue checks that a string is safe to interpolate into a CEL expression.
// Rejects values containing characters that could break or inject into CEL string literals.
func validateCELValue(value, fieldName string) error {
	if strings.ContainsAny(value, `"\`) {
		return fmt.Errorf("%s %q contains characters unsafe for CEL expressions (double-quote or backslash)", fieldName, value)
	}
	return nil
}

// findAllSubscriptionsForModel returns all MaaSSubscriptions that reference the given model,
// excluding subscriptions that are being deleted.
// Uses the field index for efficient lookup instead of cluster-wide scans.
func findAllSubscriptionsForModel(ctx context.Context, c client.Reader, modelNamespace, modelName string) ([]maasv1alpha1.MaaSSubscription, error) {
	var allSubs maasv1alpha1.MaaSSubscriptionList
	// Use field index to query subscriptions by model reference
	modelKey := modelNamespace + "/" + modelName
	if err := c.List(ctx, &allSubs, client.MatchingFields{"spec.modelRef": modelKey}); err != nil {
		return nil, fmt.Errorf("failed to list MaaSSubscriptions for model %s: %w", modelKey, err)
	}
	// Filter out subscriptions that are being deleted
	var result []maasv1alpha1.MaaSSubscription
	for _, s := range allSubs.Items {
		if !s.GetDeletionTimestamp().IsZero() {
			continue
		}
		result = append(result, s)
	}
	return result, nil
}

// findAllAuthPoliciesForModel returns all MaaSAuthPolicies that reference the given model,
// excluding policies that are being deleted.
func findAllAuthPoliciesForModel(ctx context.Context, c client.Reader, modelNamespace, modelName string) ([]maasv1alpha1.MaaSAuthPolicy, error) {
	var allPolicies maasv1alpha1.MaaSAuthPolicyList
	if err := c.List(ctx, &allPolicies); err != nil {
		return nil, fmt.Errorf("failed to list MaaSAuthPolicies: %w", err)
	}
	var result []maasv1alpha1.MaaSAuthPolicy
	for _, p := range allPolicies.Items {
		if !p.GetDeletionTimestamp().IsZero() {
			continue
		}
		for _, ref := range p.Spec.ModelRefs {
			if ref.Namespace == modelNamespace && ref.Name == modelName {
				result = append(result, p)
				break
			}
		}
	}
	return result, nil
}

// findAnySubscriptionForModel returns any one non-deleted MaaSSubscription that references the model.
// Used by watch mappers to find a subscription to trigger reconciliation for a model.
func findAnySubscriptionForModel(ctx context.Context, c client.Reader, modelNamespace, modelName string) *maasv1alpha1.MaaSSubscription {
	subs, err := findAllSubscriptionsForModel(ctx, c, modelNamespace, modelName)
	if err != nil || len(subs) == 0 {
		return nil
	}
	return &subs[0]
}

// findAnyAuthPolicyForModel returns any one non-deleted MaaSAuthPolicy that references the model.
func findAnyAuthPolicyForModel(ctx context.Context, c client.Reader, modelNamespace, modelName string) *maasv1alpha1.MaaSAuthPolicy {
	policies, err := findAllAuthPoliciesForModel(ctx, c, modelNamespace, modelName)
	if err != nil || len(policies) == 0 {
		return nil
	}
	return &policies[0]
}

// isTenantNamespace returns true when ns is either the legacy default namespace
// or, when tenant namespace discovery is enabled, a namespace labeled by the
// AITenant reconciler. Objects in unlabeled namespaces are ignored by the MaaS
// reconcilers. An empty default namespace keeps older unit-test construction
// behavior, but production startup always sets the default namespace flag.
func isTenantNamespace(ctx context.Context, c client.Reader, ns, defaultTenantNamespace string, discoveryEnabled bool) bool {
	ok, err := tenantNamespaceAllowed(ctx, c, ns, defaultTenantNamespace, discoveryEnabled)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "failed to check tenant namespace; treating namespace as non-tenant", "namespace", ns)
		return false
	}
	return ok
}

func tenantNamespaceAllowed(ctx context.Context, c client.Reader, ns, defaultTenantNamespace string, discoveryEnabled bool) (bool, error) {
	if defaultTenantNamespace == "" || ns == defaultTenantNamespace {
		return true, nil
	}
	if !discoveryEnabled {
		return false, nil
	}
	var namespace corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: ns}, &namespace); err != nil {
		if apierrors.IsNotFound(err) {
			ctrl.LoggerFrom(ctx).V(1).Info("namespace not found while checking tenant discovery label", "namespace", ns)
			return false, nil
		}
		return false, fmt.Errorf("failed to read namespace %s for tenant discovery: %w", ns, err)
	}
	return namespaceHasTenantDiscoveryLabel(namespace.Labels), nil
}

type tenantForNamespaceResult struct {
	config *maasv1alpha1.MaasTenantConfig
	legacy *maasv1alpha1.Tenant
}

func (t *tenantForNamespaceResult) identifier() (string, error) {
	switch {
	case t.config != nil:
		return tenantreconcile.TenantIdentifierFor(t.config)
	case t.legacy != nil:
		return tenantreconcile.TenantIdentifierFor(t.legacy)
	default:
		return "", errors.New("tenant config lookup result is empty")
	}
}

func (t *tenantForNamespaceResult) platformContext(
	ctx context.Context,
	c client.Reader,
	fallback maasv1alpha1.TenantGatewayRef,
) (tenantreconcile.PlatformContext, error) {
	switch {
	case t.config != nil:
		return tenantreconcile.ResolvePlatformContext(ctx, c, t.config, fallback)
	case t.legacy != nil:
		return tenantreconcile.ResolvePlatformContext(ctx, c, t.legacy, fallback)
	default:
		return tenantreconcile.PlatformContext{}, errors.New("tenant config lookup result is empty")
	}
}

// fetchTenantForNamespace returns the namespace-local tenant config lookup.
// MaasTenantConfig is preferred; legacy Tenant is returned only for migration compatibility.
func fetchTenantForNamespace(ctx context.Context, c client.Reader, namespace string) (*tenantForNamespaceResult, error) {
	config := &maasv1alpha1.MaasTenantConfig{}
	key := client.ObjectKey{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: namespace}
	if err := c.Get(ctx, key, config); err == nil {
		return &tenantForNamespaceResult{config: config}, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}

	legacy := &maasv1alpha1.Tenant{}
	legacyKey := client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: namespace}
	if err := c.Get(ctx, legacyKey, legacy); err != nil {
		return nil, err
	}
	return &tenantForNamespaceResult{legacy: legacy}, nil
}

func filterSubscriptionsByTenantNamespace(ctx context.Context, c client.Reader, subscriptions []maasv1alpha1.MaaSSubscription, defaultTenantNamespace string, discoveryEnabled bool) []maasv1alpha1.MaaSSubscription {
	allowed := make(map[string]bool)
	result := make([]maasv1alpha1.MaaSSubscription, 0, len(subscriptions))
	for _, sub := range subscriptions {
		ok, found := allowed[sub.Namespace]
		if !found {
			ok = isTenantNamespace(ctx, c, sub.Namespace, defaultTenantNamespace, discoveryEnabled)
			allowed[sub.Namespace] = ok
		}
		if ok {
			result = append(result, sub)
		}
	}
	return result
}

func filterAuthPoliciesByTenantNamespace(ctx context.Context, c client.Reader, policies []maasv1alpha1.MaaSAuthPolicy, defaultTenantNamespace string, discoveryEnabled bool) []maasv1alpha1.MaaSAuthPolicy {
	allowed := make(map[string]bool)
	result := make([]maasv1alpha1.MaaSAuthPolicy, 0, len(policies))
	for _, policy := range policies {
		ok, found := allowed[policy.Namespace]
		if !found {
			ok = isTenantNamespace(ctx, c, policy.Namespace, defaultTenantNamespace, discoveryEnabled)
			allowed[policy.Namespace] = ok
		}
		if ok {
			result = append(result, policy)
		}
	}
	return result
}

func qualifiedName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}

func hasTenantDiscoveryLabel(obj client.Object) bool {
	return namespaceHasTenantDiscoveryLabel(obj.GetLabels())
}

func namespaceHasTenantDiscoveryLabel(labels map[string]string) bool {
	return labels[tenantreconcile.LabelAIGatewayTenant] != "" ||
		labels[tenantreconcile.LabelManagedByAITenant] == "true"
}

func annotationListContains(value, want string) bool {
	if value == "" || want == "" {
		return false
	}
	for item := range strings.SplitSeq(value, ",") {
		if strings.TrimSpace(item) == want {
			return true
		}
	}
	return false
}

func tenantGatewayRefForNamespace(
	ctx context.Context,
	c client.Reader,
	tenantNamespace string,
	defaultTenantNamespace string,
	fallbackGatewayName string,
	fallbackGatewayNamespace string,
	discoveryEnabled bool,
) (maasv1alpha1.TenantGatewayRef, error) {
	tenant, err := fetchTenantForNamespace(ctx, c, tenantNamespace)
	if err == nil {
		platformContext, err := tenant.platformContext(
			ctx,
			c,
			fallbackTenantGatewayRef(fallbackGatewayName, fallbackGatewayNamespace),
		)
		if err != nil {
			return maasv1alpha1.TenantGatewayRef{}, err
		}
		return platformContext.GatewayRef, nil
	}
	if apierrors.IsNotFound(err) && (tenantNamespace == defaultTenantNamespace || !discoveryEnabled) {
		return fallbackTenantGatewayRef(fallbackGatewayName, fallbackGatewayNamespace), nil
	}
	if apierrors.IsNotFound(err) {
		allowed, allowErr := tenantNamespaceAllowed(ctx, c, tenantNamespace, defaultTenantNamespace, discoveryEnabled)
		if allowErr != nil {
			return maasv1alpha1.TenantGatewayRef{}, allowErr
		}
		if !allowed {
			return fallbackTenantGatewayRef(fallbackGatewayName, fallbackGatewayNamespace), nil
		}
		return maasv1alpha1.TenantGatewayRef{}, fmt.Errorf("MaasTenantConfig %s/%s not found for discovered tenant namespace", tenantNamespace, maasv1alpha1.MaasTenantConfigInstanceName)
	}
	return maasv1alpha1.TenantGatewayRef{}, err
}

func fallbackTenantGatewayRef(name, namespace string) maasv1alpha1.TenantGatewayRef {
	if name == "" || namespace == "" {
		return maasv1alpha1.TenantGatewayRef{}
	}
	return maasv1alpha1.TenantGatewayRef{Name: name, Namespace: namespace}
}

func validateHTTPRouteReferencesGateway(ctx context.Context, c client.Reader, routeName, routeNamespace string, gatewayRef maasv1alpha1.TenantGatewayRef) error {
	if gatewayRef.Name == "" && gatewayRef.Namespace == "" {
		return nil
	}
	if gatewayRef.Name == "" || gatewayRef.Namespace == "" {
		return errors.New("gatewayRef must set both name and namespace")
	}

	route := &gatewayapiv1.HTTPRoute{}
	if err := c.Get(ctx, client.ObjectKey{Name: routeName, Namespace: routeNamespace}, route); err != nil {
		return fmt.Errorf("failed to get HTTPRoute %s/%s for gateway validation: %w", routeNamespace, routeName, err)
	}
	if len(route.Spec.ParentRefs) > maxHTTPRouteParentRefs {
		return fmt.Errorf("HTTPRoute %s/%s has %d parentRefs, exceeding supported maximum %d",
			routeNamespace, routeName, len(route.Spec.ParentRefs), maxHTTPRouteParentRefs)
	}
	for _, parentRef := range route.Spec.ParentRefs {
		if !parentRefTargetsGateway(parentRef) {
			continue
		}
		parentNamespace := routeNamespace
		if parentRef.Namespace != nil {
			parentNamespace = string(*parentRef.Namespace)
		}
		if string(parentRef.Name) == gatewayRef.Name && parentNamespace == gatewayRef.Namespace {
			return nil
		}
	}
	return fmt.Errorf("HTTPRoute %s/%s does not reference tenant Gateway %s/%s", routeNamespace, routeName, gatewayRef.Namespace, gatewayRef.Name)
}

const (
	maxHTTPRouteParentRefs         = 32
	gatewayAPIParentRefKindGateway = "Gateway"
)

// parentRefTargetsGateway reports whether parentRef refers to a Gateway API Gateway.
// Omitted kind/group use Gateway API defaults (kind=Gateway, group=gateway.networking.k8s.io).
func parentRefTargetsGateway(parentRef gatewayapiv1.ParentReference) bool {
	parentKind := gatewayAPIParentRefKindGateway
	if parentRef.Kind != nil {
		parentKind = string(*parentRef.Kind)
	}
	parentGroup := string(gatewayapiv1.GroupName)
	if parentRef.Group != nil {
		parentGroup = string(*parentRef.Group)
	}
	return parentKind == gatewayAPIParentRefKindGateway && parentGroup == string(gatewayapiv1.GroupName)
}
