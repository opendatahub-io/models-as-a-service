package maas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	httpguts "golang.org/x/net/http/httpguts"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

const subscriptionHeadersAnnotation = "experimental.maas.opendatahub.io/headers"

func (r *MaaSAuthPolicyReconciler) subscriptionHeaderNames(ctx context.Context, namespace string) ([]string, error) {
	var subscriptions maasv1alpha1.MaaSSubscriptionList
	if err := r.List(ctx, &subscriptions, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list MaaSSubscriptions in %s for header configuration: %w", namespace, err)
	}

	names := make(map[string]struct{})
	for i := range subscriptions.Items {
		headers, err := parseSubscriptionHeaders(subscriptions.Items[i].GetAnnotations()[subscriptionHeadersAnnotation])
		if err != nil {
			r.warnInvalidSubscriptionHeaders(ctx, &subscriptions.Items[i], err)
			continue
		}
		for name := range headers {
			names[strings.ToLower(name)] = struct{}{}
		}
	}

	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

func applySubscriptionHeaders(spec map[string]any, names []string) error {
	headers, found, err := unstructured.NestedMap(spec, "defaults", "rules", "response", "success", "headers")
	if err != nil {
		return fmt.Errorf("read gateway response headers: %w", err)
	}
	if !found {
		return errors.New("gateway response headers are missing")
	}
	managedNames := managedSubscriptionHeaderNames(names)
	for name := range headers {
		lowerName := strings.ToLower(name)
		if _, managed := managedNames[lowerName]; managedSubscriptionHeaderName(lowerName) && !managed {
			delete(headers, name)
		}
	}
	for _, name := range names {
		headers[name] = subscriptionInfoHeadersExpr(name)
	}
	if err := unstructured.SetNestedMap(spec, headers, "defaults", "rules", "response", "success", "headers"); err != nil {
		return err
	}

	patterns, found, err := unstructured.NestedSlice(
		spec, "defaults", "rules", "authorization", "deny-client-identity-headers", "patternMatching", "patterns",
	)
	if err != nil {
		return fmt.Errorf("read gateway identity-header deny patterns: %w", err)
	}
	if !found {
		return errors.New("gateway identity-header deny patterns are missing")
	}
	patterns = append(patterns, map[string]any{"predicate": subscriptionHeadersDenyPredicate(names)})
	return unstructured.SetNestedSlice(
		spec, patterns, "defaults", "rules", "authorization", "deny-client-identity-headers", "patternMatching", "patterns",
	)
}

func basePolicyHasStaleSubscriptionHeaderFields(current map[string]any, names []string) bool {
	headers, found, err := unstructured.NestedMap(current, "defaults", "rules", "response", "success", "headers")
	if err != nil || !found {
		return false
	}

	managedNames := managedSubscriptionHeaderNames(names)
	for name := range headers {
		lowerName := strings.ToLower(name)
		if _, managed := managedNames[lowerName]; managedSubscriptionHeaderName(lowerName) && !managed {
			return true
		}
	}
	return false
}

func managedSubscriptionHeaderName(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "x-grid-")
}

func managedSubscriptionHeaderNames(names []string) map[string]struct{} {
	managedNames := make(map[string]struct{}, len(names))
	for _, name := range names {
		managedNames[name] = struct{}{}
	}
	return managedNames
}

func subscriptionHeadersDenyPredicate(names []string) string {
	if len(names) == 0 {
		return `request.headers.all(name, !name.startsWith("x-grid-"))`
	}

	encodedNames, err := json.Marshal(names)
	if err != nil {
		panic(fmt.Sprintf("marshal subscription header names: %v", err))
	}
	return fmt.Sprintf(
		`request.headers.all(name, !name.startsWith("x-grid-") || name in %s)`,
		encodedNames,
	)
}

func subscriptionInfoHeadersExpr(name string) map[string]any {
	encodedName := strconv.Quote(name)
	return map[string]any{
		"plain": map[string]any{
			"expression": fmt.Sprintf(
				`has(auth.metadata) && "subscription-info" in auth.metadata && `+
					`"headers" in auth.metadata["subscription-info"] && %s in auth.metadata["subscription-info"].headers `+
					`? auth.metadata["subscription-info"].headers[%s].join(", ") : ""`,
				encodedName, encodedName,
			),
		},
		"metrics":  false,
		"priority": int64(0),
	}
}

// The experimental subscription-header annotation is a contract shared with
// maas-api/internal/subscription/subscription_headers.go. Keep parsing and
// validation semantics in sync so selection and rendered AuthPolicies agree.
func parseSubscriptionHeaders(value string) (map[string][]string, error) {
	if value == "" {
		return nil, nil
	}

	var headers map[string][]string
	if err := json.Unmarshal([]byte(value), &headers); err != nil {
		return nil, err
	}
	if headers == nil {
		return nil, errors.New("subscription headers must be a JSON object")
	}

	for name := range headers {
		if !validSubscriptionHeaderName(name) {
			return nil, fmt.Errorf("invalid header name %q", name)
		}
		values := headers[name]
		if values == nil {
			return nil, fmt.Errorf("null values for header %q", name)
		}
		for _, headerValue := range values {
			if !validSubscriptionHeaderValue(headerValue) {
				return nil, fmt.Errorf("invalid value for header %q", name)
			}
		}
	}

	normalized := make(map[string][]string, len(headers))
	for name, values := range headers {
		lowerName := strings.ToLower(name)
		if _, exists := normalized[lowerName]; exists {
			return nil, fmt.Errorf("duplicate case-insensitive header name %q", lowerName)
		}
		normalized[lowerName] = values
	}
	return normalized, nil
}

func (r *MaaSAuthPolicyReconciler) warnInvalidSubscriptionHeaders(
	ctx context.Context, subscription *maasv1alpha1.MaaSSubscription, parseErr error,
) {
	logr.FromContextOrDiscard(ctx).Error(
		parseErr, "Ignoring invalid subscription headers annotation",
		"subscription", subscription.Namespace+"/"+subscription.Name,
	)
	if r.Recorder != nil {
		r.Recorder.Eventf(
			subscription, nil, corev1.EventTypeWarning,
			"InvalidSubscriptionHeaders", "SubscriptionHeaderNames",
			"Ignoring invalid subscription headers annotation: %v", parseErr,
		)
	}
}

func validSubscriptionHeaderName(name string) bool {
	if !httpguts.ValidHeaderFieldName(name) {
		return false
	}

	lowerName := strings.ToLower(name)
	return strings.HasPrefix(lowerName, "x-grid-")
}

func validSubscriptionHeaderValue(value string) bool {
	return httpguts.ValidHeaderFieldValue(value)
}
