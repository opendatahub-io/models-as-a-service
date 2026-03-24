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

package model_provider_resolver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/bbr/framework"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/plugin"

	"github.com/opendatahub-io/ai-gateway-payload-processing/pkg/plugins/common/state"
)

const (
	ModelProviderResolverPluginType = "model-provider-resolver"
)

// maasModelRefGVK is the GroupVersionKind for MaaSModelRef CRD.
var maasModelRefGVK = schema.GroupVersionKind{
	Group:   "maas.opendatahub.io",
	Version: "v1alpha1",
	Kind:    "MaaSModelRef",
}

// compile-time type validation
var _ framework.RequestProcessor = &ModelProviderResolverPlugin{}

// ModelProviderResolverFactory defines the factory function for ModelProviderResolverPlugin
func ModelProviderResolverFactory(name string, _ json.RawMessage, handle framework.Handle) (framework.BBRPlugin, error) {
	plugin, err := NewModelProviderResolver(handle.ReconcilerBuilder, handle.ClientReader())
	if err != nil {
		return nil, fmt.Errorf("failed to create plugin '%s' - %w", ModelProviderResolverPluginType, err)
	}

	return plugin.WithName(name), nil
}

func NewModelProviderResolver(reconcilerBuilder func() *builder.Builder, clientReader client.Reader) (*ModelProviderResolverPlugin, error) {
	modelInfoStore := newModelInfoStore()
	reconciler := &maasModelRefReconciler{
		Reader: clientReader,
		store:  modelInfoStore,
	}

	// Watch MaaSModelRef CRDs using unstructured client (no MaaS type dependency)
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(maasModelRefGVK)

	if err := reconcilerBuilder().For(obj).Complete(reconciler); err != nil {
		return nil, fmt.Errorf("failed to register MaaSModelRef reconciler for plugin '%s' - %w", ModelProviderResolverPluginType, err)
	}

	return &ModelProviderResolverPlugin{
		typedName:      plugin.TypedName{Type: ModelProviderResolverPluginType, Name: ModelProviderResolverPluginType},
		modelInfoStore: modelInfoStore,
	}, nil
}

// ModelProviderResolverPlugin resolves model names to provider info by watching MaaSModelRef CRDs.
// It writes the model, provider and credential reference to CycleState for downstream plugins
// (api-translation, api-key-injection).
type ModelProviderResolverPlugin struct {
	typedName      plugin.TypedName
	modelInfoStore *modelInfoStore
}

// TypedName returns the type and name tuple of this plugin instance.
func (p *ModelProviderResolverPlugin) TypedName() plugin.TypedName {
	return p.typedName
}

// WithName sets the name of the plugin instance.
func (p *ModelProviderResolverPlugin) WithName(name string) *ModelProviderResolverPlugin {
	p.typedName.Name = name
	return p
}

// ProcessRequest reads the model name from the request body, resolves the provider
// from the modelInfoStore (populated by MaaSModelRef reconciler), and writes model, provider
// and credential reference info to CycleState.
func (p *ModelProviderResolverPlugin) ProcessRequest(ctx context.Context, cycleState *framework.CycleState, request *framework.InferenceRequest) error {
	if request == nil || request.Headers == nil || request.Body == nil {
		return fmt.Errorf("invalid inference request: request/headers/body must be non-nil")
	}

	model, ok := request.Body["model"].(string)
	if !ok || model == "" {
		return errors.New("failed to read 'model' from request body")
	}

	info, found := p.modelInfoStore.getModelInfo(model)
	if !found { // info is stored only for external models
		return nil // this is not considered an error, we just need to skip if it's internal model
	}

	// info of external model written to cycle state for next plugins
	cycleState.Write(state.ModelKey, model)
	cycleState.Write(state.ProviderKey, info.provider)
	if info.credentialRefName != "" {
		cycleState.Write(state.CredsRefName, info.credentialRefName)
	}
	if info.credentialRefNamespace != "" {
		cycleState.Write(state.CredsRefNamespace, info.credentialRefNamespace)
	}

	return nil
}
