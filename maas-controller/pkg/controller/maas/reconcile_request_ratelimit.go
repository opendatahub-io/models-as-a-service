/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package maas

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

var gvkRateLimitPolicy = schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "RateLimitPolicy"}

// reconcileRLPForModel builds or updates the aggregated RateLimitPolicy that
// enforces request-count limits on /v1/rerank for each model HTTPRoute.
func (r *MaaSSubscriptionReconciler) reconcileRLPForModel(ctx context.Context, log logr.Logger, modelNamespace, modelName string) error {
	allSubs, err := findAllSubscriptionsForModel(ctx, r.Client, modelNamespace, modelName)
	if err != nil {
		return fmt.Errorf("failed to list subscriptions for model %s/%s: %w", modelNamespace, modelName, err)
	}
	allSubs = filterSubscriptionsByTenantNamespace(ctx, r.Client, allSubs, r.DefaultTenantNamespace, r.TenantNamespaceDiscoveryEnabled)

	httpRouteName, httpRouteNS, err := findHTTPRouteForModel(ctx, r.Client, modelNamespace, modelName)
	if err != nil {
		if errors.Is(err, ErrModelNotFound) || len(allSubs) == 0 {
			log.V(1).Info("model/route not found during cleanup, deleting RateLimitPolicy via labels", "model", modelNamespace+"/"+modelName, "error", err.Error())
			if delErr := r.deleteModelRLP(ctx, log, modelNamespace, modelName); delErr != nil {
				return fmt.Errorf("failed to clean up RateLimitPolicy for missing model %s/%s: %w", modelNamespace, modelName, delErr)
			}
			return nil
		}
		if errors.Is(err, ErrHTTPRouteNotFound) {
			log.V(1).Info("HTTPRoute not found for model, skipping RateLimitPolicy creation", "model", modelNamespace+"/"+modelName)
			return nil
		}
		return fmt.Errorf("failed to resolve HTTPRoute for model %s/%s: %w", modelNamespace, modelName, err)
	}
	allSubs, skipped, err := r.validateSubscriptionTenantGatewaysForRoute(ctx, allSubs, httpRouteName, httpRouteNS, modelNamespace, modelName)
	if err != nil {
		return err
	}
	if len(skipped) > 0 {
		log.V(1).Info("Skipping subscriptions not attached to the model route tenant gateway",
			"model", modelNamespace+"/"+modelName,
			"skippedSubscriptions", skipped)
	}

	policyName := fmt.Sprintf("maas-rlp-%s", modelName)
	existingCheck := &unstructured.Unstructured{}
	existingCheck.SetGroupVersionKind(gvkRateLimitPolicy)
	existingCheck.SetName(policyName)
	existingCheck.SetNamespace(httpRouteNS)
	if err := r.Get(ctx, client.ObjectKeyFromObject(existingCheck), existingCheck); err == nil {
		if !isManaged(existingCheck) {
			log.V(1).Info("RateLimitPolicy opted out, skipping reconciliation", "name", policyName, "namespace", httpRouteNS, "model", modelNamespace+"/"+modelName)
			return nil
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to check existing RateLimitPolicy: %w", err)
	}

	if len(allSubs) == 0 {
		log.V(1).Info("no active subscriptions for model, deleting RateLimitPolicy", "model", modelNamespace+"/"+modelName)
		if delErr := r.deleteModelRLP(ctx, log, modelNamespace, modelName); delErr != nil {
			return fmt.Errorf("failed to delete RateLimitPolicy for model %s/%s: %w", modelNamespace, modelName, delErr)
		}
		return nil
	}

	route := &gatewayapiv1.HTTPRoute{}
	if err := r.Get(ctx, types.NamespacedName{Name: httpRouteName, Namespace: httpRouteNS}, route); err != nil {
		return fmt.Errorf("failed to fetch HTTPRoute %s/%s: %w", httpRouteNS, httpRouteName, err)
	}

	trlpGatewayName := ""
	trlpGatewayNamespace := ""
	if gateway, ok, err := gatewayIdentityForTRLP(ctx, r.Client, modelNamespace, modelName, route); err != nil {
		return fmt.Errorf("failed to resolve selected gateway for model %s/%s: %w", modelNamespace, modelName, err)
	} else if ok {
		trlpGatewayName = gateway.Name
		trlpGatewayNamespace = gateway.Namespace
	}

	var subs []subInfo
	for _, sub := range allSubs {
		for _, mRef := range sub.Spec.ModelRefs {
			if mRef.Namespace != modelNamespace || mRef.Name != modelName {
				continue
			}
			rates, unlimited, err := modelRefTokenRates(mRef)
			if err != nil {
				log.Error(err, "Skipping subscription with invalid token budget for request limits",
					"subscription", sub.Name, "model", modelNamespace+"/"+modelName)
				continue
			}
			subs = append(subs, subInfo{
				subNamespace: sub.Namespace,
				subName:      sub.Name,
				rates:        rates,
				unlimited:    unlimited,
				modelScoped:  fmt.Sprintf("%s/%s@%s/%s", sub.Namespace, sub.Name, mRef.Namespace, mRef.Name),
				groupKey:     rateGroupKey(rates),
				legacy:       classifiedRateLimitIdentity(&sub) == RateLimitIdentityLegacy,
			})
			break
		}
	}

	if len(subs) == 0 && len(allSubs) > 0 {
		log.Info("All subscriptions for model have invalid rate limits — deleting RateLimitPolicy",
			"model", modelNamespace+"/"+modelName, "invalidCount", len(allSubs))
		return r.deleteModelRLP(ctx, log, modelNamespace, modelName)
	}

	limitsMap, subNames := buildRequestCountGroupedLimits(subs)

	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(gvkRateLimitPolicy)
	policy.SetName(policyName)
	policy.SetNamespace(httpRouteNS)
	labels := map[string]string{
		trlpLabelModel:                 modelName,
		trlpLabelModelNamespace:        modelNamespace,
		"app.kubernetes.io/managed-by": "maas-controller",
		"app.kubernetes.io/part-of":    "maas-subscription",
		"app.kubernetes.io/component":  "request-rate-limit-policy",
	}
	if trlpGatewayName != "" && trlpGatewayNamespace != "" {
		labels[trlpLabelGatewayName] = trlpGatewayName
		labels[trlpLabelGatewayNamespace] = trlpGatewayNamespace
	}
	policy.SetLabels(labels)
	policy.SetAnnotations(map[string]string{
		"maas.opendatahub.io/subscriptions": strings.Join(subNames, ","),
	})

	if err := setHTTPRouteControllerOwner(route, policy, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference on RateLimitPolicy %s/%s: %w", policy.GetNamespace(), policy.GetName(), err)
	}

	spec := map[string]any{
		"targetRef": map[string]any{
			"group": "gateway.networking.k8s.io",
			"kind":  "HTTPRoute",
			"name":  httpRouteName,
		},
		"limits": limitsMap,
	}
	if err := unstructured.SetNestedMap(policy.Object, spec, "spec"); err != nil {
		return fmt.Errorf("failed to set spec: %w", err)
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(policy.GroupVersionKind())
	err = r.Get(ctx, client.ObjectKeyFromObject(policy), existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, policy); err != nil {
			if apierrors.IsAlreadyExists(err) || apimeta.IsNoMatchError(err) {
				log.V(1).Info("RateLimitPolicy CRD not available or already exists", "name", policyName, "error", err.Error())
				return nil
			}
			return fmt.Errorf("failed to create RateLimitPolicy for model %s: %w", modelName, err)
		}
		log.Info("RateLimitPolicy created", "name", policyName, "model", modelName, "subscriptionCount", len(subNames))
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get existing RateLimitPolicy: %w", err)
	}

	if err := r.updateExistingRLP(ctx, log, route, policy, spec, policyName, modelNamespace, modelName, subNames); err != nil {
		return fmt.Errorf("failed to update RateLimitPolicy for model %s/%s: %w", modelNamespace, modelName, err)
	}
	return nil
}

func (r *MaaSSubscriptionReconciler) updateExistingRLP(
	ctx context.Context,
	log logr.Logger,
	route *gatewayapiv1.HTTPRoute,
	policy *unstructured.Unstructured,
	spec map[string]any,
	policyName, modelNamespace, modelName string,
	subNames []string,
) error {
	return retry.OnError(retry.DefaultBackoff, apierrors.IsConflict, func() error {
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(policy.GroupVersionKind())
		reader := r.APIReader
		if reader == nil {
			reader = r.Client
		}
		if err := reader.Get(ctx, client.ObjectKeyFromObject(policy), existing); err != nil {
			return err
		}
		if !isManaged(existing) {
			log.V(1).Info("RateLimitPolicy opted out during reconciliation, skipping update", "name", policyName)
			return nil
		}
		snapshot := existing.DeepCopy()
		if err := setHTTPRouteControllerOwner(route, existing, r.Scheme); err != nil {
			return fmt.Errorf("failed to set owner reference on existing RateLimitPolicy %s/%s: %w", existing.GetNamespace(), existing.GetName(), err)
		}

		mergedAnnotations := existing.GetAnnotations()
		if mergedAnnotations == nil {
			mergedAnnotations = make(map[string]string)
		}
		for k, v := range policy.GetAnnotations() {
			mergedAnnotations[k] = v
		}
		existing.SetAnnotations(mergedAnnotations)

		mergedLabels := existing.GetLabels()
		if mergedLabels == nil {
			mergedLabels = make(map[string]string)
		}
		for k, v := range policy.GetLabels() {
			mergedLabels[k] = v
		}
		existing.SetLabels(mergedLabels)
		if err := unstructured.SetNestedMap(existing.Object, spec, "spec"); err != nil {
			return fmt.Errorf("failed to update spec: %w", err)
		}

		if equality.Semantic.DeepEqual(snapshot.Object, existing.Object) {
			log.V(1).Info("RateLimitPolicy unchanged, skipping update", "name", policyName, "model", modelNamespace+"/"+modelName)
			return nil
		}
		if err := r.Update(ctx, existing); err != nil {
			return err
		}
		log.Info("RateLimitPolicy updated", "name", policyName, "model", modelNamespace+"/"+modelName, "subscriptionCount", len(subNames))
		return nil
	})
}

func (r *MaaSSubscriptionReconciler) deleteModelRLP(ctx context.Context, log logr.Logger, modelNamespace, modelName string) error {
	policyList := &unstructured.UnstructuredList{}
	policyList.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "RateLimitPolicyList"})
	labelSelector := client.MatchingLabels{
		trlpLabelModel:                 modelName,
		trlpLabelModelNamespace:        modelNamespace,
		"app.kubernetes.io/managed-by": "maas-controller",
		"app.kubernetes.io/part-of":    "maas-subscription",
	}
	if err := r.List(ctx, policyList, labelSelector); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("failed to list RateLimitPolicy for cleanup: %w", err)
	}
	for i := range policyList.Items {
		p := &policyList.Items[i]
		if !isManaged(p) {
			continue
		}
		log.V(1).Info("Deleting RateLimitPolicy", "name", p.GetName(), "namespace", p.GetNamespace(), "model", modelNamespace+"/"+modelName)
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete RateLimitPolicy %s/%s: %w", p.GetNamespace(), p.GetName(), err)
		}
	}
	return nil
}
