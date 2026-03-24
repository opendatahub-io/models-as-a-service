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
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/bbr/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/plugin"

	"github.com/opendatahub-io/ai-gateway-payload-processing/pkg/plugins/common/provider"
	"github.com/opendatahub-io/ai-gateway-payload-processing/pkg/plugins/common/state"
)

const (
	// APIKeyInjectionPluginType is the registered name for this plugin in the BBR registry.
	APIKeyInjectionPluginType = "apikey-injection"

	// managedLabel selects Secrets managed by the apikey-injection plugin.
	// Only Secrets carrying this label are watched by the reconciler.
	managedLabel = "inference.networking.k8s.io/bbr-managed"
)

// compile-time interface check
var _ framework.RequestProcessor = &ApiKeyInjectionPlugin{}

// apiKeyGenerator generates a single auth header from an API key.
// headerName is the HTTP header (e.g. "Authorization", "x-api-key").
// headerValuePrefix is prepended to the key (e.g. "Bearer "); use "" for raw keys.
type apiKeyGenerator struct {
	headerName        string
	headerValuePrefix string
}

// generateHeader returns the header name and formatted value for the given API key.
func (inj *apiKeyGenerator) generateHeader(apiKey string) (string, string) {
	return inj.headerName, inj.headerValuePrefix + apiKey
}

// defaultApiKeyGenerators returns the built-in provider-to-generator registry.
func defaultApiKeyGenerators() map[string]*apiKeyGenerator {
	return map[string]*apiKeyGenerator{
		provider.OpenAI:      {headerName: "Authorization", headerValuePrefix: "Bearer "},
		provider.Anthropic:   {headerName: "x-api-key"},
		provider.AzureOpenAI: {headerName: "api-key"},
		provider.Vertex:      {headerName: "Authorization", headerValuePrefix: "Bearer "},
	}
}

// APIKeyInjectionFactory creates a new apiKeyInjectionPlugin from CLI parameters and
// registers its Secret reconciler via the Handle.
// It matches the framework.FactoryFunc signature.
func APIKeyInjectionFactory(name string, _ json.RawMessage, handle framework.Handle) (framework.BBRPlugin, error) {
	plugin, err := NewAPIKeyInjectionPlugin(handle.ReconcilerBuilder, handle.ClientReader())
	if err != nil {
		return nil, fmt.Errorf("failed to create plugin '%s' - %w", APIKeyInjectionPluginType, err)
	}

	return plugin.WithName(name), nil
}

func NewAPIKeyInjectionPlugin(reconcilerBuilder func() *builder.Builder, clientReader client.Reader) (*ApiKeyInjectionPlugin, error) {
	store := newSecretStore()
	reconciler := &secretReconciler{
		Reader: clientReader,
		store:  store,
	}

	labelPredicate, err := predicate.LabelSelectorPredicate(metav1.LabelSelector{
		MatchLabels: map[string]string{managedLabel: "true"},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to build label predicate for plugin '%s' - %w", APIKeyInjectionPluginType, err)
	}

	if err := reconcilerBuilder().For(&corev1.Secret{}).WithEventFilter(labelPredicate).Complete(reconciler); err != nil {
		return nil, fmt.Errorf("failed to register Secret reconciler for plugin '%s' - %w", APIKeyInjectionPluginType, err)
	}

	return (&ApiKeyInjectionPlugin{
		typedName: plugin.TypedName{
			Type: APIKeyInjectionPluginType,
			Name: APIKeyInjectionPluginType,
		},
		apikeyGenerators: defaultApiKeyGenerators(),
		store:            store,
	}), nil
}

// ApiKeyInjectionPlugin injects an API key from a Kubernetes Secret
// into the request headers. The Secret is identified by its namespaced
// name from CycleState. The provider (openai, anthropic) determines
// which header name and value format are used.
type ApiKeyInjectionPlugin struct {
	typedName        plugin.TypedName
	apikeyGenerators map[string]*apiKeyGenerator
	store            *secretStore
}

// TypedName returns the type and name tuple of this plugin instance.
func (p *ApiKeyInjectionPlugin) TypedName() plugin.TypedName {
	return p.typedName
}

// WithName sets the name of this plugin instance.
func (p *ApiKeyInjectionPlugin) WithName(name string) *ApiKeyInjectionPlugin {
	p.typedName.Name = name
	return p
}

// ProcessRequest reads the credential Secret reference and provider from
// CycleState (written by provider-resolver), looks up the API key in the
// store, and injects provider-specific auth headers into the request.
func (p *ApiKeyInjectionPlugin) ProcessRequest(ctx context.Context, cycleState *framework.CycleState, request *framework.InferenceRequest) error {
	if request == nil || request.Headers == nil {
		return fmt.Errorf("request or headers is nil")
	}

	credsName, err := framework.ReadCycleStateKey[string](cycleState, state.CredsRefName)
	if err != nil || credsName == "" {
		return fmt.Errorf("missing credentials reference name in CycleState")
	}
	credsNamespace, err := framework.ReadCycleStateKey[string](cycleState, state.CredsRefNamespace)
	if err != nil || credsNamespace == "" {
		return fmt.Errorf("missing credentials reference namespace in CycleState")
	}

	secretKey := fmt.Sprintf("%s/%s", credsNamespace, credsName)
	apiKey, found := p.store.get(secretKey)
	if !found {
		return fmt.Errorf("no secret found for ref '%s'", secretKey)
	}

	providerName, _ := framework.ReadCycleStateKey[string](cycleState, state.ProviderKey)
	generator, ok := p.apikeyGenerators[providerName]
	if !ok {
		generator = p.apikeyGenerators[provider.OpenAI]
	}

	headerName, headerValue := generator.generateHeader(apiKey)
	request.SetHeader(headerName, headerValue) // inject the generated header

	log.FromContext(ctx).Info("API key injected", "secretRef", secretKey, "provider", providerName)
	return nil
}
