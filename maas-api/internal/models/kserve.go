package models

import (
	"errors"
	"fmt"

	kservev1beta1 "github.com/kserve/kserve/pkg/apis/serving/v1beta1"
	kservelistersv1alpha1 "github.com/kserve/kserve/pkg/client/listers/serving/v1alpha1"
	kservelistersv1beta1 "github.com/kserve/kserve/pkg/client/listers/serving/v1beta1"
	"github.com/openai/openai-go/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"knative.dev/pkg/apis"
	gatewaylisters "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

type Manager struct {
	isvcLister      kservelistersv1beta1.InferenceServiceLister
	llmIsvcLister   kservelistersv1alpha1.LLMInferenceServiceLister
	httpRouteLister gatewaylisters.HTTPRouteLister
	gatewayRef      GatewayRef
	logger          *logger.Logger
}

func NewManager(
	log *logger.Logger,
	isvcLister kservelistersv1beta1.InferenceServiceLister,
	llmIsvcLister kservelistersv1alpha1.LLMInferenceServiceLister,
	httpRouteLister gatewaylisters.HTTPRouteLister,
	gatewayRef GatewayRef,
) (*Manager, error) {
	if isvcLister == nil {
		return nil, errors.New("isvcLister is required")
	}
	if llmIsvcLister == nil {
		return nil, errors.New("llmIsvcLister is required")
	}
	if httpRouteLister == nil {
		return nil, errors.New("httpRouteLister is required")
	}

	return &Manager{
		isvcLister:      isvcLister,
		llmIsvcLister:   llmIsvcLister,
		httpRouteLister: httpRouteLister,
		gatewayRef:      gatewayRef,
		logger:          log,
	}, nil
}

// ListAvailableModels lists all InferenceServices across all namespaces.
func (m *Manager) ListAvailableModels() ([]Model, error) {
	list, err := m.isvcLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list InferenceServices: %w", err)
	}

	return m.inferenceServicesToModels(list)
}

func (m *Manager) inferenceServicesToModels(items []*kservev1beta1.InferenceService) ([]Model, error) {
	models := make([]Model, 0, len(items))

	for _, item := range items {
		url := m.findInferenceServiceURL(item)
		if url == nil {
			m.logger.Debug("Failed to find URL for InferenceService")
		}

		modelID := item.Name
		if item.Spec.Predictor.Model != nil && item.Spec.Predictor.Model.ModelFormat.Name != "" {
			modelID = item.Spec.Predictor.Model.ModelFormat.Name
		}

		models = append(models, Model{
			Model: openai.Model{
				ID:      modelID,
				Object:  "model",
				OwnedBy: item.Namespace,
				Created: item.CreationTimestamp.Unix(),
			},
			URL:   url,
			Ready: m.checkInferenceServiceReadiness(item),
		})
	}

	return models, nil
}

func (m *Manager) findInferenceServiceURL(is *kservev1beta1.InferenceService) *apis.URL {
	if is.Status.URL != nil {
		return is.Status.URL
	}

	if is.Status.Address != nil && is.Status.Address.URL != nil {
		return is.Status.Address.URL
	}

	m.logger.Debug("No URL found for InferenceService")
	return nil
}

func (m *Manager) checkInferenceServiceReadiness(is *kservev1beta1.InferenceService) bool {
	if is.DeletionTimestamp != nil {
		return false
	}

	if is.Generation > 0 && is.Status.ObservedGeneration != is.Generation {
		m.logger.Debug("ObservedGeneration is stale, not ready yet",
			"observed_generation", is.Status.ObservedGeneration,
			"expected_generation", is.Generation,
		)
		return false
	}

	if len(is.Status.Conditions) == 0 {
		m.logger.Debug("No conditions found for InferenceService")
		return false
	}

	for _, cond := range is.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			return false
		}
	}

	return true
}
