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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/bbr/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/plugin"

	"github.com/opendatahub-io/ai-gateway-payload-processing/pkg/plugins/common/provider"
	"github.com/opendatahub-io/ai-gateway-payload-processing/pkg/plugins/common/state"
)

// newTestPlugin creates an apiKeyInjectionPlugin for unit tests, bypassing the
// Handle-based Factory (which requires a real manager).
func newTestPlugin(store *secretStore) *ApiKeyInjectionPlugin {
	return &ApiKeyInjectionPlugin{
		typedName:        plugin.TypedName{Type: APIKeyInjectionPluginType, Name: APIKeyInjectionPluginType},
		apikeyGenerators: defaultApiKeyGenerators(),
		store:            store,
	}
}

// newTestRequest builds an InferenceRequest pre-populated with the given headers.
func newTestRequest(headers map[string]string) *framework.InferenceRequest {
	req := framework.NewInferenceRequest()
	for k, v := range headers {
		req.Headers[k] = v
	}
	return req
}

// newCycleState builds a CycleState with credential ref and optional provider.
func newCycleState(namespace, name, providerName string) *framework.CycleState {
	cs := framework.NewCycleState()
	cs.Write(state.CredsRefName, name)
	cs.Write(state.CredsRefNamespace, namespace)
	if providerName != "" {
		cs.Write(state.ProviderKey, providerName)
	}
	return cs
}

func TestProcessRequest(t *testing.T) {
	tests := []struct {
		name        string
		secrets     []*corev1.Secret
		cycleState  *framework.CycleState
		wantHeaders map[string]string
	}{
		{
			name:       "OpenAI provider — injects Authorization: Bearer",
			secrets:    []*corev1.Secret{testSecret("default", "openai-key", "sk-test-key")},
			cycleState: newCycleState("default", "openai-key", provider.OpenAI),
			wantHeaders: map[string]string{
				"Authorization": "Bearer sk-test-key",
			},
		},
		{
			name:       "Anthropic provider — injects x-api-key with raw value",
			secrets:    []*corev1.Secret{testSecret("default", "anthropic-key", "ant-key-123")},
			cycleState: newCycleState("default", "anthropic-key", provider.Anthropic),
			wantHeaders: map[string]string{
				"x-api-key": "ant-key-123",
			},
		},
		{
			name:       "Vertex provider — injects Authorization: Bearer (same as OpenAI)",
			secrets:    []*corev1.Secret{testSecret("default", "vertex-key", "vtx-key-456")},
			cycleState: newCycleState("default", "vertex-key", provider.Vertex),
			wantHeaders: map[string]string{
				"Authorization": "Bearer vtx-key-456",
			},
		},
		{
			name:       "default provider when CycleState has no provider — uses OpenAI Bearer",
			secrets:    []*corev1.Secret{testSecret("default", "no-provider", "sk-key")},
			cycleState: newCycleState("default", "no-provider", ""),
			wantHeaders: map[string]string{
				"Authorization": "Bearer sk-key",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := seedStore(tt.secrets...)
			p := newTestPlugin(store)
			req := newTestRequest(nil)

			err := p.ProcessRequest(context.Background(), tt.cycleState, req)
			require.NoError(t, err)
			for k, v := range tt.wantHeaders {
				assert.Equal(t, v, req.Headers[k])
			}
		})
	}
}

func TestProcessRequestMissingCredsRef(t *testing.T) {
	store := seedStore(testSecret("default", "key", "sk-key"))
	p := newTestPlugin(store)
	req := newTestRequest(nil)
	cs := framework.NewCycleState()

	err := p.ProcessRequest(context.Background(), cs, req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing credentials reference")
}

func TestProcessRequestSecretNotFound(t *testing.T) {
	store := newSecretStore()
	p := newTestPlugin(store)
	req := newTestRequest(nil)
	cs := newCycleState("default", "unknown", provider.OpenAI)

	err := p.ProcessRequest(context.Background(), cs, req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no secret found for ref")
}

func TestProcessRequestUnknownProviderFallback(t *testing.T) {
	store := seedStore(testSecret("default", "some-key", "key-789"))
	p := newTestPlugin(store)
	req := newTestRequest(nil)
	cs := newCycleState("default", "some-key", "unknown-provider")

	err := p.ProcessRequest(context.Background(), cs, req)
	require.NoError(t, err)
	assert.Equal(t, "Bearer key-789", req.Headers["Authorization"])
}

func TestProcessRequestNilRequest(t *testing.T) {
	store := newSecretStore()
	p := newTestPlugin(store)

	err := p.ProcessRequest(context.Background(), nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "request or headers is nil")
}

func TestProcessRequestMutationTracking(t *testing.T) {
	store := seedStore(testSecret("default", "key", "sk-key"))
	p := newTestPlugin(store)
	req := newTestRequest(nil)
	cs := newCycleState("default", "key", provider.OpenAI)

	err := p.ProcessRequest(context.Background(), cs, req)
	require.NoError(t, err)

	mutated := req.MutatedHeaders()
	assert.Equal(t, "Bearer sk-key", mutated["Authorization"],
		"SetHeader should register the injected header in MutatedHeaders()")
}

func TestTypedName(t *testing.T) {
	p := newTestPlugin(nil)
	assert.Equal(t, APIKeyInjectionPluginType, p.TypedName().Type)
	assert.Equal(t, APIKeyInjectionPluginType, p.TypedName().Name)
}

func TestDefaultInjectors(t *testing.T) {
	injectors := defaultApiKeyGenerators()

	require.Contains(t, injectors, provider.OpenAI)
	assert.Equal(t, "Authorization", injectors[provider.OpenAI].headerName)
	assert.Equal(t, "Bearer ", injectors[provider.OpenAI].headerValuePrefix)

	require.Contains(t, injectors, provider.Anthropic)
	assert.Equal(t, "x-api-key", injectors[provider.Anthropic].headerName)
	assert.Empty(t, injectors[provider.Anthropic].headerValuePrefix)

	require.Contains(t, injectors, provider.AzureOpenAI)
	assert.Equal(t, "api-key", injectors[provider.AzureOpenAI].headerName)
	assert.Empty(t, injectors[provider.AzureOpenAI].headerValuePrefix)

	require.Contains(t, injectors, provider.Vertex)
	assert.Equal(t, "Authorization", injectors[provider.Vertex].headerName)
	assert.Equal(t, "Bearer ", injectors[provider.Vertex].headerValuePrefix)

	assert.Len(t, injectors, 4)
}

func TestAPIKeyInjector(t *testing.T) {
	tests := []struct {
		name            string
		headerName      string
		headerPrefix    string
		apiKey          string
		wantHeaderName  string
		wantHeaderValue string
	}{
		{
			name:            "Bearer prefix (OpenAI style)",
			headerName:      "Authorization",
			headerPrefix:    "Bearer ",
			apiKey:          "sk-test-key",
			wantHeaderName:  "Authorization",
			wantHeaderValue: "Bearer sk-test-key",
		},
		{
			name:            "raw key without prefix (Anthropic style)",
			headerName:      "x-api-key",
			apiKey:          "ant-key-123",
			wantHeaderName:  "x-api-key",
			wantHeaderValue: "ant-key-123",
		},
		{
			name:            "custom header name",
			headerName:      "api-key",
			apiKey:          "some-key-456",
			wantHeaderName:  "api-key",
			wantHeaderValue: "some-key-456",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			injector := &apiKeyGenerator{
				headerName:        tt.headerName,
				headerValuePrefix: tt.headerPrefix,
			}

			gotName, gotValue := injector.generateHeader(tt.apiKey)

			assert.Equal(t, tt.wantHeaderName, gotName)
			assert.Equal(t, tt.wantHeaderValue, gotValue)
		})
	}
}
