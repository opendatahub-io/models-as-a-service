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

package apikey_injection

import (
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
)

// secretDataKey is the key within Secret.Data that holds the API key value.
const secretDataKey = "api-key"

// secretStore is a thread-safe in-memory store that maps a Secret's
// namespaced name ("namespace/name") to its API key value.
// The secretReconciler writes to it; the apiKeyInjectionPlugin reads from it.
type secretStore struct {
	mu   sync.RWMutex
	data map[string]string
}

// newSecretStore creates an empty secretStore.
func newSecretStore() *secretStore {
	return &secretStore{
		data: make(map[string]string),
	}
}

// addOrUpdate extracts the API key from the Secret's data field and stores
// it under the given key.
// Returns an error if the Secret is missing the required api-key data field.
func (s *secretStore) addOrUpdate(key string, secret *corev1.Secret) error {
	apiKeyBytes, ok := secret.Data[secretDataKey]
	if !ok || len(apiKeyBytes) == 0 {
		return fmt.Errorf("secret %q missing %q data field", key, secretDataKey)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = string(apiKeyBytes)
	return nil
}

// delete removes the entry for the given Secret namespaced name.
func (s *secretStore) delete(secretKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, secretKey)
}

// get returns the API key for the given namespaced name and whether it was found.
func (s *secretStore) get(secretKey string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	apiKey, ok := s.data[secretKey]
	return apiKey, ok
}
