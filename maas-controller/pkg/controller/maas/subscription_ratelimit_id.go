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
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// Rate-limit identity annotation. Must stay in sync with
// maas-api/internal/constant.AnnotationRateLimitIdentity.
//
// Short IDs (PR 1546) shrink Kuadrant WASM / EnvoyFilter predicates, but they
// also change Limitador counter keys. Subscriptions that existed before this
// controller version keep selected_subscription_key matching and counters so
// in-window quotas are not reset; only subscriptions created after the
// upgrade (or explicitly annotated short) use the compact ID.
const (
	AnnotationRateLimitIdentity = "maas.opendatahub.io/rate-limit-identity"
	RateLimitIdentityShort      = "short"
	RateLimitIdentityLegacy     = "legacy"
)

// ModelScopedSubscriptionKey builds the human-readable subscription@model
// identity still exposed as auth.identity.selected_subscription_key.
// Format: {subNamespace}/{subName}@{modelNamespace}/{modelName}
func ModelScopedSubscriptionKey(subNamespace, subName, modelNamespace, modelName string) string {
	return fmt.Sprintf("%s/%s@%s/%s", subNamespace, subName, modelNamespace, modelName)
}

// SubscriptionRateLimitID returns a short stable ID for TokenRateLimitPolicy
// when-predicates and counters. It is the first 16 hex chars of SHA-256 over
// ModelScopedSubscriptionKey(...). Must stay in sync with
// maas-api/internal/subscription.RateLimitID (same input, same digest).
// Limit map keys stay rate-grouped (tokens-<limit>-per-<window>); only the
// identity clauses use this short ID.
func SubscriptionRateLimitID(modelScopedKey string) string {
	sum := sha256.Sum256([]byte(modelScopedKey))
	return hex.EncodeToString(sum[:8])
}

// classifiedRateLimitIdentity returns the sticky identity scheme for a
// subscription. An explicit annotation always wins. Otherwise a CR that
// already has status (survived an upgrade) stays legacy; a brand-new CR with
// empty status is short.
func classifiedRateLimitIdentity(sub *maasv1alpha1.MaaSSubscription) string {
	if sub == nil {
		return RateLimitIdentityLegacy
	}
	if sub.Annotations != nil {
		switch sub.Annotations[AnnotationRateLimitIdentity] {
		case RateLimitIdentityShort, RateLimitIdentityLegacy:
			return sub.Annotations[AnnotationRateLimitIdentity]
		}
	}
	if sub.Status.Phase != "" || len(sub.Status.Conditions) > 0 {
		return RateLimitIdentityLegacy
	}
	return RateLimitIdentityShort
}

// stampRateLimitIdentityAnnotation writes the classified scheme when the
// annotation is missing or unknown. Returns true when metadata changed and
// needs an Update. Once set, the value is sticky so later status updates
// cannot flip a new subscription to legacy.
func stampRateLimitIdentityAnnotation(sub *maasv1alpha1.MaaSSubscription) bool {
	if sub == nil {
		return false
	}
	want := classifiedRateLimitIdentity(sub)
	if sub.Annotations != nil && sub.Annotations[AnnotationRateLimitIdentity] == want {
		return false
	}
	if sub.Annotations == nil {
		sub.Annotations = map[string]string{}
	}
	sub.Annotations[AnnotationRateLimitIdentity] = want
	return true
}

func subscriptionSelectClause(si subInfo) string {
	if si.legacy {
		return fmt.Sprintf(`auth.identity.selected_subscription_key == "%s"`, si.modelScoped)
	}
	return fmt.Sprintf(`auth.identity.selected_subscription_id == "%s"`, SubscriptionRateLimitID(si.modelScoped))
}

func subscriptionCounterExpression(legacy bool) string {
	if legacy {
		return "auth.identity.selected_subscription_key"
	}
	return "auth.identity.selected_subscription_id"
}
