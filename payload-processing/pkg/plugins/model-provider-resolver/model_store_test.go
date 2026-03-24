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
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/types"

	"github.com/opendatahub-io/ai-gateway-payload-processing/pkg/plugins/common/provider"
)

func TestModelStore_SetAndGet(t *testing.T) {
	store := newModelInfoStore()
	key := types.NamespacedName{Name: "test", Namespace: "ns"}

	store.setModelInfo("model-a", ModelInfo{provider: provider.Anthropic}, key)

	info, found := store.getModelInfo("model-a")
	assert.True(t, found)
	assert.Equal(t, provider.Anthropic, info.provider)
}

func TestModelStore_DeleteByResource(t *testing.T) {
	store := newModelInfoStore()
	key := types.NamespacedName{Name: "test", Namespace: "ns"}

	store.setModelInfo("model-a", ModelInfo{provider: provider.Anthropic}, key)
	store.deleteByResource(key)

	_, found := store.getModelInfo("model-a")
	assert.False(t, found)
}

func TestModelStore_DeleteNonExistent(t *testing.T) {
	store := newModelInfoStore()
	// should not panic
	store.deleteByResource(types.NamespacedName{Name: "nonexistent", Namespace: "ns"})
}
