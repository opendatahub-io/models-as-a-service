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

package model_provider_resolver

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/bbr/framework"

	"github.com/opendatahub-io/ai-gateway-payload-processing/pkg/plugins/common/provider"
	"github.com/opendatahub-io/ai-gateway-payload-processing/pkg/plugins/common/state"
)

func TestProcessRequest_ModelResolved(t *testing.T) {
	store := newModelInfoStore()
	model := "claude-sonnet"
	credentialRefName := "anthropic-key"
	credentialRefNamespace := "llm"
	store.setModelInfo(model, ModelInfo{
		provider:               provider.Anthropic,
		credentialRefName:      credentialRefName,
		credentialRefNamespace: credentialRefNamespace,
	}, types.NamespacedName{Name: "claude-sonnet", Namespace: "llm"})

	plugin := &ModelProviderResolverPlugin{modelInfoStore: store}
	cs := framework.NewCycleState()
	req := framework.NewInferenceRequest()
	req.Body["model"] = model

	err := plugin.ProcessRequest(context.Background(), cs, req)
	require.NoError(t, err)

	actualModel, err := framework.ReadCycleStateKey[string](cs, state.ModelKey)
	assert.NoError(t, err)
	assert.Equal(t, model, actualModel)

	actualProvider, err := framework.ReadCycleStateKey[string](cs, state.ProviderKey)
	assert.NoError(t, err)
	assert.Equal(t, provider.Anthropic, actualProvider)

	actualCredsName, err := framework.ReadCycleStateKey[string](cs, state.CredsRefName)
	assert.NoError(t, err)
	assert.Equal(t, credentialRefName, actualCredsName)

	actualCredsNamespace, err := framework.ReadCycleStateKey[string](cs, state.CredsRefNamespace)
	assert.NoError(t, err)
	assert.Equal(t, credentialRefNamespace, actualCredsNamespace)
}

func TestProcessRequest_ModelNotFound(t *testing.T) {
	store := newModelInfoStore()
	p := &ModelProviderResolverPlugin{modelInfoStore: store}
	cs := framework.NewCycleState()
	req := framework.NewInferenceRequest()
	req.Body["model"] = "unknown-model"

	err := p.ProcessRequest(context.Background(), cs, req)
	assert.NoError(t, err)

	_, provErr := framework.ReadCycleStateKey[string](cs, state.ProviderKey)
	assert.Error(t, provErr) // not found in CycleState
}

func TestProcessRequest_NoModel(t *testing.T) {
	store := newModelInfoStore()
	p := &ModelProviderResolverPlugin{modelInfoStore: store}

	err := p.ProcessRequest(context.Background(), framework.NewCycleState(), framework.NewInferenceRequest())
	assert.Error(t, err)
}

func TestProcessRequest_NilRequest(t *testing.T) {
	store := newModelInfoStore()
	p := &ModelProviderResolverPlugin{modelInfoStore: store}

	err := p.ProcessRequest(context.Background(), framework.NewCycleState(), nil)
	assert.Error(t, err)
}

func TestProcessRequest_NoCredentialRef(t *testing.T) {
	store := newModelInfoStore()
	store.setModelInfo("gpt-4o", ModelInfo{
		provider: provider.OpenAI,
		// no credential ref
	}, types.NamespacedName{Name: "gpt-4o", Namespace: "llm"})

	p := &ModelProviderResolverPlugin{modelInfoStore: store}
	cs := framework.NewCycleState()
	req := framework.NewInferenceRequest()
	req.Body["model"] = "gpt-4o"

	err := p.ProcessRequest(context.Background(), cs, req)
	require.NoError(t, err)

	actualProvider, _ := framework.ReadCycleStateKey[string](cs, state.ProviderKey)
	assert.Equal(t, provider.OpenAI, actualProvider)

	_, credErr := framework.ReadCycleStateKey[string](cs, state.CredsRefName)
	assert.Error(t, credErr)
}
