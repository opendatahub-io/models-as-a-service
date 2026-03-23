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
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestReconcile(t *testing.T) {
	tests := []struct {
		name       string
		secret     *corev1.Secret
		preSeed    []*corev1.Secret
		wantKey    string
		wantAPIKey string
		wantFound  bool
		wantErr    bool
		secretName string
	}{
		{
			name:       "stores API key from Secret",
			secret:     testSecret("default", "openai-key", "sk-live-xxx"),
			wantKey:    "default/openai-key",
			wantAPIKey: "sk-live-xxx",
			wantFound:  true,
		},
		{
			name:       "updates existing entry on Secret change",
			secret:     testSecret("default", "openai-key", "sk-new-key"),
			preSeed:    []*corev1.Secret{testSecret("default", "openai-key", "sk-old-key")},
			wantKey:    "default/openai-key",
			wantAPIKey: "sk-new-key",
			wantFound:  true,
		},
		{
			name:       "Secret not found — cleans store",
			secret:     nil,
			secretName: "gone",
			preSeed:    []*corev1.Secret{testSecret("default", "gone", "sk-key")},
			wantKey:    "default/gone",
			wantFound:  false,
		},
		{
			name: "Secret missing api-key data — returns error",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-data",
					Namespace: "default",
				},
				Data: map[string][]byte{},
			},
			wantKey:   "default/no-data",
			wantFound: false,
			wantErr:   true,
		},
		{
			name: "Secret marked for deletion — removes from store",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "deleting",
					Namespace:         "default",
					DeletionTimestamp: &metav1.Time{Time: time.Now()},
					Finalizers:        []string{"test-finalizer"},
				},
				Data: map[string][]byte{
					secretDataKey: []byte("sk-key"),
				},
			},
			preSeed:   []*corev1.Secret{testSecret("default", "deleting", "sk-key")},
			wantKey:   "default/deleting",
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := seedStore(tt.preSeed...)

			builder := fake.NewClientBuilder()
			if tt.secret != nil {
				builder = builder.WithObjects(tt.secret)
			}
			fakeClient := builder.Build()

			reconciler := &secretReconciler{
				Reader: fakeClient,
				store:  store,
			}

			name := tt.secretName
			if name == "" && tt.secret != nil {
				name = tt.secret.Name
			}
			if name == "" {
				name = "test-secret"
			}

			ns := "default"
			if tt.secret != nil {
				ns = tt.secret.Namespace
			}

			_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      name,
					Namespace: ns,
				},
			})

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)

			if tt.wantKey != "" {
				apiKey, found := store.get(tt.wantKey)
				assert.Equal(t, tt.wantFound, found)
				if tt.wantFound {
					assert.Equal(t, tt.wantAPIKey, apiKey)
				}
			}
		})
	}
}
