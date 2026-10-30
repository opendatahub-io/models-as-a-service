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
	"slices"
	"time"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// rateLimitIdentityEpoch is when this controller process started. Unannotated
// subscriptions that already had status and were created before this instant
// are treated as upgrade survivors (legacy). CRs created after process start
// stay short even if status lands before the sticky identity is written —
// otherwise a reconcile race would grandfather brand-new subscriptions onto
// selected_subscription_key and defeat the short-ID size win.
var rateLimitIdentityEpoch = time.Now()

// Rate-limit identity values. Must stay in sync with
// maas-api/internal/constant.RateLimitIdentity*.
//
// Short IDs (PR 1546) shrink Kuadrant WASM / EnvoyFilter predicates, but they
// also change Limitador counter keys. Subscriptions that existed before this
// controller version keep selected_subscription_key matching and counters so
// in-window quotas are not reset; only subscriptions created after the
// upgrade (or explicitly opted into short via spec) use the compact ID.
//
// The sticky effective value lives on status.rateLimitIdentity (GitOps-safe).
// Optional spec.rateLimitIdentity is the operator escape hatch.
const (
	// AnnotationRateLimitIdentity is the pre-status stamp used during early
	// PR testing. Still honored once for migration into status; never written.
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

func recognizedRateLimitIdentity(v string) bool {
	return v == RateLimitIdentityShort || v == RateLimitIdentityLegacy
}

// classifiedRateLimitIdentity returns the sticky identity scheme for a
// subscription. Callers that will AddFinalizer must classify before mutating
// the in-memory object so a brand-new CR is not mistaken for an upgrade
// survivor.
//
// Precedence:
//  1. spec.rateLimitIdentity (operator escape hatch)
//  2. status.rateLimitIdentity (sticky, GitOps-safe)
//  3. legacy annotation (one-time migration from earlier PR builds)
//  4. persisted cleanup finalizer → legacy
//  5. empty phase/conditions → short (genuinely new)
//  6. creation before process-start epoch → legacy; otherwise short
func classifiedRateLimitIdentity(sub *maasv1alpha1.MaaSSubscription) string {
	if sub == nil {
		return RateLimitIdentityLegacy
	}
	if recognizedRateLimitIdentity(sub.Spec.RateLimitIdentity) {
		return sub.Spec.RateLimitIdentity
	}
	if recognizedRateLimitIdentity(sub.Status.RateLimitIdentity) {
		return sub.Status.RateLimitIdentity
	}
	if sub.Annotations != nil {
		if v := sub.Annotations[AnnotationRateLimitIdentity]; recognizedRateLimitIdentity(v) {
			return v
		}
	}
	if slices.Contains(sub.Finalizers, maasSubscriptionFinalizer) {
		return RateLimitIdentityLegacy
	}
	if sub.Status.Phase == "" && len(sub.Status.Conditions) == 0 {
		return RateLimitIdentityShort
	}
	created := sub.CreationTimestamp.Time
	if sub.CreationTimestamp.IsZero() || created.Before(rateLimitIdentityEpoch) {
		return RateLimitIdentityLegacy
	}
	return RateLimitIdentityShort
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
