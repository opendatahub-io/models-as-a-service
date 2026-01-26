package models

import (
	"context"
	"fmt"
	"net/url"
	"sync"

	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"knative.dev/pkg/apis"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
)

// maxDiscoveryConcurrency limits parallel HTTP calls during model discovery
// to avoid overwhelming model servers or hitting rate limits.
const maxDiscoveryConcurrency = 10

type GatewayRef struct {
	Name      string
	Namespace string
}

// ListAvailableLLMs discovers and returns models from authorized LLMInferenceServices.
func (m *Manager) ListAvailableLLMs(ctx context.Context, saToken string) ([]Model, error) {
	list, err := m.llmIsvcLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("failed to list LLMInferenceServices: %w", err)
	}

	var instanceLLMs []*kservev1alpha1.LLMInferenceService
	for _, llmIsvc := range list {
		if m.partOfMaaSInstance(llmIsvc) {
			instanceLLMs = append(instanceLLMs, llmIsvc)
		}
	}

	var (
		authorizedModels []Model
		mu               sync.Mutex
	)

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(maxDiscoveryConcurrency)

	for _, llmIsvc := range instanceLLMs {
		g.Go(func() error {
			models := m.discoverModels(ctx, llmIsvc, saToken)
			if len(models) > 0 {
				mu.Lock()
				defer mu.Unlock()
				authorizedModels = append(authorizedModels, models...)
			}
			return nil
		})
	}

	_ = g.Wait() // errors handled within goroutines

	return authorizedModels, nil
}

func (m *Manager) discoverModels(ctx context.Context, llmIsvc *kservev1alpha1.LLMInferenceService, saToken string) []Model {
	svcMetadata := m.extractMetadata(llmIsvc)
	if svcMetadata.URL == nil {
		m.logger.Debug("LLMInferenceService has no URL, skipping",
			"namespace", llmIsvc.Namespace,
			"name", llmIsvc.Name,
		)
		return nil
	}

	endpoint, err := url.JoinPath(svcMetadata.URL.String(), "/v1/models")
	if err != nil {
		m.logger.Error("Failed to create endpoint URL",
			"namespace", llmIsvc.Namespace,
			"name", llmIsvc.Name,
			"error", err,
		)
		return nil
	}

	discoveredModels := m.fetchModelsWithRetry(ctx, endpoint, saToken, svcMetadata)
	if discoveredModels == nil {
		return nil
	}

	return m.enrichModels(discoveredModels, svcMetadata)
}

func (m *Manager) extractMetadata(llmIsvc *kservev1alpha1.LLMInferenceService) llmInferenceServiceMetadata {
	return llmInferenceServiceMetadata{
		ServiceName: llmIsvc.Name,
		ModelName:   m.extractModelName(llmIsvc),
		URL:         m.findLLMInferenceServiceURL(llmIsvc),
		Ready:       m.checkLLMInferenceServiceReadiness(llmIsvc),
		Details:     m.extractModelDetails(llmIsvc),
		Namespace:   llmIsvc.Namespace,
		Created:     llmIsvc.CreationTimestamp.Unix(),
	}
}

func (m *Manager) extractModelName(llmIsvc *kservev1alpha1.LLMInferenceService) string {
	if llmIsvc.Spec.Model.Name != nil && *llmIsvc.Spec.Model.Name != "" {
		return *llmIsvc.Spec.Model.Name
	}
	return llmIsvc.Name
}

// partOfMaaSInstance checks if the given LLMInferenceService is part of this "MaaS instance". This means that it is
// either directly referenced by the gateway that has MaaS capabilities, or it is referenced by an HTTPRoute that is managed by the gateway.
// The gateway is part of the component configuration.
func (m *Manager) partOfMaaSInstance(llmIsvc *kservev1alpha1.LLMInferenceService) bool {
	if llmIsvc.Spec.Router == nil {
		return false
	}

	return m.hasDirectGatewayReference(llmIsvc) ||
		m.hasHTTPRouteSpecRefToGateway(llmIsvc) ||
		m.hasReferencedRouteAttachedToGateway(llmIsvc) ||
		m.hasManagedRouteAttachedToGateway(llmIsvc)
}

func (m *Manager) findLLMInferenceServiceURL(llmIsvc *kservev1alpha1.LLMInferenceService) *apis.URL {
	if llmIsvc.Status.URL != nil {
		return llmIsvc.Status.URL
	}

	if llmIsvc.Status.Address != nil && llmIsvc.Status.Address.URL != nil {
		return llmIsvc.Status.Address.URL
	}

	if len(llmIsvc.Status.Addresses) > 0 {
		return llmIsvc.Status.Addresses[0].URL
	}

	m.logger.Debug("No URL found for LLMInferenceService",
		"namespace", llmIsvc.Namespace,
		"name", llmIsvc.Name,
	)
	return nil
}

func (m *Manager) extractModelDetails(llmIsvc *kservev1alpha1.LLMInferenceService) *Details {
	annotations := llmIsvc.GetAnnotations()
	if annotations == nil {
		return nil
	}

	genaiUseCase := annotations[constant.AnnotationGenAIUseCase]
	description := annotations[constant.AnnotationDescription]
	displayName := annotations[constant.AnnotationDisplayName]

	// Only return Details if at least one field is populated
	if genaiUseCase == "" && description == "" && displayName == "" {
		return nil
	}

	return &Details{
		GenAIUseCase: genaiUseCase,
		Description:  description,
		DisplayName:  displayName,
	}
}

func (m *Manager) checkLLMInferenceServiceReadiness(llmIsvc *kservev1alpha1.LLMInferenceService) bool {
	if llmIsvc.DeletionTimestamp != nil {
		return false
	}

	if llmIsvc.Generation > 0 && llmIsvc.Status.ObservedGeneration != llmIsvc.Generation {
		m.logger.Debug("ObservedGeneration is stale, not ready yet",
			"observed_generation", llmIsvc.Status.ObservedGeneration,
			"expected_generation", llmIsvc.Generation,
		)
		return false
	}

	if len(llmIsvc.Status.Conditions) == 0 {
		m.logger.Debug("No conditions found for LLMInferenceService",
			"namespace", llmIsvc.Namespace,
			"name", llmIsvc.Name,
		)
		return false
	}

	for _, cond := range llmIsvc.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			return false
		}
	}

	return true
}

func (m *Manager) hasDirectGatewayReference(llmIsvc *kservev1alpha1.LLMInferenceService) bool {
	if llmIsvc.Spec.Router.Gateway == nil {
		return false
	}

	for _, ref := range llmIsvc.Spec.Router.Gateway.Refs {
		if string(ref.Name) != m.gatewayRef.Name {
			continue
		}

		refNamespace := llmIsvc.Namespace
		if ref.Namespace != "" {
			refNamespace = string(ref.Namespace)
		}

		if refNamespace == m.gatewayRef.Namespace {
			return true
		}
	}

	return false
}

func (m *Manager) hasHTTPRouteSpecRefToGateway(llmIsvc *kservev1alpha1.LLMInferenceService) bool {
	if llmIsvc.Spec.Router.Route == nil || llmIsvc.Spec.Router.Route.HTTP == nil || llmIsvc.Spec.Router.Route.HTTP.Spec == nil {
		return false
	}

	for _, parentRef := range llmIsvc.Spec.Router.Route.HTTP.Spec.ParentRefs {
		if string(parentRef.Name) != m.gatewayRef.Name {
			continue
		}

		parentNamespace := llmIsvc.Namespace
		if parentRef.Namespace != nil {
			parentNamespace = string(*parentRef.Namespace)
		}

		if parentNamespace == m.gatewayRef.Namespace {
			return true
		}
	}

	return false
}

func (m *Manager) hasReferencedRouteAttachedToGateway(llmIsvc *kservev1alpha1.LLMInferenceService) bool {
	if llmIsvc.Spec.Router.Route == nil || llmIsvc.Spec.Router.Route.HTTP == nil || len(llmIsvc.Spec.Router.Route.HTTP.Refs) == 0 {
		return false
	}

	for _, routeRef := range llmIsvc.Spec.Router.Route.HTTP.Refs {
		route, err := m.httpRouteLister.HTTPRoutes(llmIsvc.Namespace).Get(routeRef.Name)
		if err != nil {
			m.logger.Debug("HTTPRoute not in cache",
				"namespace", llmIsvc.Namespace,
				"name", routeRef.Name,
				"error", err,
			)
			continue
		}
		if route == nil {
			continue
		}

		if m.routeAttachedToGateway(route, llmIsvc.Namespace) {
			return true
		}
	}

	return false
}

func (m *Manager) hasManagedRouteAttachedToGateway(llmIsvc *kservev1alpha1.LLMInferenceService) bool {
	if llmIsvc.Spec.Router.Route == nil || llmIsvc.Spec.Router.Route.HTTP == nil {
		return false
	}

	httpRoute := llmIsvc.Spec.Router.Route.HTTP
	if httpRoute.Spec != nil || len(httpRoute.Refs) > 0 {
		return false
	}

	selector := labels.SelectorFromSet(labels.Set{
		"app.kubernetes.io/component": "llminferenceservice-router",
		"app.kubernetes.io/name":      llmIsvc.Name,
		"app.kubernetes.io/part-of":   "llminferenceservice",
	})

	routes, err := m.httpRouteLister.HTTPRoutes(llmIsvc.Namespace).List(selector)
	if err != nil {
		m.logger.Debug("Failed to list HTTPRoutes for LLM",
			"namespace", llmIsvc.Namespace,
			"name", llmIsvc.Name,
			"error", err,
		)
		return false
	}

	for _, route := range routes {
		if m.routeAttachedToGateway(route, llmIsvc.Namespace) {
			return true
		}
	}

	return false
}

func (m *Manager) routeAttachedToGateway(route *gwapiv1.HTTPRoute, defaultNamespace string) bool {
	for _, parentRef := range route.Spec.ParentRefs {
		if string(parentRef.Name) != m.gatewayRef.Name {
			continue
		}

		parentNamespace := defaultNamespace
		if parentRef.Namespace != nil {
			parentNamespace = string(*parentRef.Namespace)
		}

		if parentNamespace == m.gatewayRef.Namespace {
			return true
		}
	}

	return false
}
