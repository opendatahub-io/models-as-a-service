/*
Copyright 2026 The opendatahub.io Authors.

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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// testSecret builds a corev1.Secret for test seeding.
func testSecret(namespace, name, apiKey string) *corev1.Secret {
	if namespace == "" {
		namespace = "default"
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			secretDataKey: []byte(apiKey),
		},
	}
}

// seedStore is a convenience for tests that need a pre-populated secretStore.
func seedStore(secrets ...*corev1.Secret) *secretStore {
	s := newSecretStore()
	for _, sec := range secrets {
		_ = s.addOrUpdate(fmt.Sprintf("%s/%s", sec.Namespace, sec.Name), sec)
	}
	return s
}

func TestSecretStore(t *testing.T) {
	tests := []struct {
		name          string
		initStoreFunc func(t *testing.T, s *secretStore)
	}{
		{
			name: "AddOrUpdate and Get returns stored API key",
			initStoreFunc: func(t *testing.T, s *secretStore) {
				sec := testSecret("default", "openai-key", "sk-key-1")
				require.NoError(t, s.addOrUpdate("default/openai-key", sec))

				apiKey, found := s.get("default/openai-key")
				assert.True(t, found)
				assert.Equal(t, "sk-key-1", apiKey)
			},
		},
		{
			name: "get nonexistent key returns not found",
			initStoreFunc: func(t *testing.T, s *secretStore) {
				_, found := s.get("default/nonexistent")
				assert.False(t, found)
			},
		},
		{
			name: "AddOrUpdate overwrites existing entry",
			initStoreFunc: func(t *testing.T, s *secretStore) {
				_ = s.addOrUpdate("default/key", testSecret("default", "key", "old-key"))
				_ = s.addOrUpdate("default/key", testSecret("default", "key", "new-key"))

				apiKey, found := s.get("default/key")
				assert.True(t, found)
				assert.Equal(t, "new-key", apiKey)
			},
		},
		{
			name: "delete removes entry",
			initStoreFunc: func(t *testing.T, s *secretStore) {
				_ = s.addOrUpdate("default/key", testSecret("default", "key", "sk-key-1"))
				s.delete("default/key")

				_, found := s.get("default/key")
				assert.False(t, found)
			},
		},
		{
			name: "delete nonexistent key is a no-op",
			initStoreFunc: func(t *testing.T, s *secretStore) {
				s.delete("default/nonexistent")
			},
		},
		{
			name: "multiple secrets are independent",
			initStoreFunc: func(t *testing.T, s *secretStore) {
				_ = s.addOrUpdate("default/key-a", testSecret("default", "key-a", "value-1"))
				_ = s.addOrUpdate("default/key-b", testSecret("default", "key-b", "value-2"))

				v1, f1 := s.get("default/key-a")
				v2, f2 := s.get("default/key-b")
				assert.True(t, f1)
				assert.True(t, f2)
				assert.Equal(t, "value-1", v1)
				assert.Equal(t, "value-2", v2)

				s.delete("default/key-a")
				_, f1 = s.get("default/key-a")
				_, f2 = s.get("default/key-b")
				assert.False(t, f1)
				assert.True(t, f2)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSecretStore()
			tt.initStoreFunc(t, s)
		})
	}
}

func TestAddOrUpdate(t *testing.T) {
	tests := []struct {
		name       string
		secret     *corev1.Secret
		wantKey    string
		wantAPIKey string
		wantErr    bool
	}{
		{
			name:       "stores API key from Secret data",
			secret:     testSecret("default", "openai-key", "sk-live-xxx"),
			wantKey:    "default/openai-key",
			wantAPIKey: "sk-live-xxx",
		},
		{
			name: "returns error when api-key data is missing",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-data",
					Namespace: "default",
				},
				Data: map[string][]byte{},
			},
			wantKey: "default/no-data",
			wantErr: true,
		},
		{
			name: "returns error when api-key data is empty",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "empty-key",
					Namespace: "default",
				},
				Data: map[string][]byte{secretDataKey: []byte("")},
			},
			wantKey: "default/empty-key",
			wantErr: true,
		},
		{
			name: "overwrites existing entry on update",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "key",
					Namespace: "default",
				},
				Data: map[string][]byte{secretDataKey: []byte("new-key")},
			},
			wantKey:    "default/key",
			wantAPIKey: "new-key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSecretStore()
			if tt.name == "overwrites existing entry on update" {
				_ = s.addOrUpdate("default/key", testSecret("default", "key", "old-key"))
			}

			err := s.addOrUpdate(tt.wantKey, tt.secret)

			if tt.wantErr {
				require.Error(t, err)
				_, found := s.get(tt.wantKey)
				assert.False(t, found, "store should not contain entry when AddOrUpdate fails")
				return
			}

			require.NoError(t, err)
			apiKey, found := s.get(tt.wantKey)
			assert.True(t, found)
			assert.Equal(t, tt.wantAPIKey, apiKey)
		})
	}
}

func TestSecretStoreConcurrentAccess(t *testing.T) {
	s := newSecretStore()
	var wg sync.WaitGroup
	goroutines := 100

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := fmt.Sprintf("default/secret-%d", n)
			sec := testSecret("default", fmt.Sprintf("secret-%d", n), "key")
			_ = s.addOrUpdate(key, sec)
			s.get(key)
			s.delete(key)
		}(i)
	}
	wg.Wait()
}
