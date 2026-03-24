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
	"sync"

	"k8s.io/apimachinery/pkg/types"
)

// ModelInfo holds the provider and credential reference for an external model.
type ModelInfo struct {
	provider               string
	credentialRefName      string
	credentialRefNamespace string
}

// modelInfoStore is a thread-safe in-memory store that maps model names to their provider info.
// The reconciler writes to it; the plugin reads from it during request processing.
type modelInfoStore struct {
	models map[string]ModelInfo
	// modelRefKeyToModel tracks which MaaSModelRef resource added each model entry,
	// so we can clean up when the resource is deleted.
	modelRefKeyToModel map[types.NamespacedName]string
	lock               sync.RWMutex
}

func newModelInfoStore() *modelInfoStore {
	return &modelInfoStore{
		models:             make(map[string]ModelInfo),
		modelRefKeyToModel: make(map[types.NamespacedName]string),
	}
}

// getModelInfo returns the ModelInfo for a model name, or empty if not found.
func (s *modelInfoStore) getModelInfo(modelName string) (ModelInfo, bool) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	info, ok := s.models[modelName]
	return info, ok
}

// setModelInfo stores the model→provider mapping and records which resource it came from.
func (s *modelInfoStore) setModelInfo(modelName string, info ModelInfo, modelRefKey types.NamespacedName) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.models[modelName] = info
	s.modelRefKeyToModel[modelRefKey] = modelName
}

// deleteByResource removes the model entry that was added by the given MaaSModelRef resource.
func (s *modelInfoStore) deleteByResource(modelRefKey types.NamespacedName) {
	s.lock.Lock()
	defer s.lock.Unlock()
	modelName, ok := s.modelRefKeyToModel[modelRefKey]
	if !ok {
		return // no model info was stored for this modelRef
	}

	delete(s.models, modelName)
	delete(s.modelRefKeyToModel, modelRefKey)
}
