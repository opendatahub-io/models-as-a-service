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

package subscription

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ModelScopedKey builds {subNamespace}/{subName}@{modelNamespace}/{modelName}.
// Must match maas-controller ModelScopedSubscriptionKey.
func ModelScopedKey(subNamespace, subName, modelNamespace, modelName string) string {
	return fmt.Sprintf("%s/%s@%s/%s", subNamespace, subName, modelNamespace, modelName)
}

// RateLimitID returns the first 16 hex chars of SHA-256(modelScopedKey).
// Must stay in sync with maas-controller SubscriptionRateLimitID.
func RateLimitID(modelScopedKey string) string {
	if modelScopedKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(modelScopedKey))
	return hex.EncodeToString(sum[:8])
}

// RateLimitIDFor returns RateLimitID for a subscription + resolved model identity
// (resolvedModel is "namespace/name").
func RateLimitIDFor(subNamespace, subName, resolvedModel string) string {
	if subNamespace == "" || subName == "" || resolvedModel == "" {
		return ""
	}
	return RateLimitID(fmt.Sprintf("%s/%s@%s", subNamespace, subName, resolvedModel))
}
