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
)

// ModelScopedSubscriptionKey builds the human-readable subscription@model
// identity still exposed as auth.identity.selected_subscription_key.
// Format: {subNamespace}/{subName}@{modelNamespace}/{modelName}
func ModelScopedSubscriptionKey(subNamespace, subName, modelNamespace, modelName string) string {
	return fmt.Sprintf("%s/%s@%s/%s", subNamespace, subName, modelNamespace, modelName)
}

// SubscriptionRateLimitID returns a short stable ID for TokenRateLimitPolicy
// when-predicates and limit map keys. It is the first 16 hex chars of SHA-256
// over ModelScopedSubscriptionKey(...). Must stay in sync with
// maas-api/internal/subscription.RateLimitID (same input, same digest).
func SubscriptionRateLimitID(modelScopedKey string) string {
	sum := sha256.Sum256([]byte(modelScopedKey))
	return hex.EncodeToString(sum[:8])
}
