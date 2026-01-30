package models

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	kservelistersv1alpha1 "github.com/kserve/kserve/pkg/client/listers/serving/v1alpha1"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewaylisters "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

// HTTP client configuration for model discovery.
const (
	httpClientTimeout   = 5 * time.Second
	httpMaxIdleConns    = 100
	httpIdleConnTimeout = 90 * time.Second
)

// maxDiscoveryConcurrency limits parallel HTTP calls during model discovery
// to avoid overwhelming model servers or hitting rate limits.
const maxDiscoveryConcurrency = 10

type GatewayRef struct {
	Name      string
	Namespace string
}

type Manager struct {
	llmIsvcLister   kservelistersv1alpha1.LLMInferenceServiceLister
	httpRouteLister gatewaylisters.HTTPRouteLister
	gatewayRef      GatewayRef
	logger          *logger.Logger
	httpClient      *http.Client
}

func NewManager(
	log *logger.Logger,
	llmIsvcLister kservelistersv1alpha1.LLMInferenceServiceLister,
	httpRouteLister gatewaylisters.HTTPRouteLister,
	gatewayRef GatewayRef,
) (*Manager, error) {
	if log == nil {
		return nil, errors.New("log is required")
	}
	if llmIsvcLister == nil {
		return nil, errors.New("llmIsvcLister is required")
	}
	if httpRouteLister == nil {
		return nil, errors.New("httpRouteLister is required")
	}

	return &Manager{
		llmIsvcLister:   llmIsvcLister,
		httpRouteLister: httpRouteLister,
		gatewayRef:      gatewayRef,
		logger:          log,
		httpClient: &http.Client{
			Timeout: httpClientTimeout,
			Transport: &http.Transport{
				// TLS certificate verification is skipped for model discovery requests.
				// Security context:
				// - Traffic is cluster-internal only (gateway loopback to LLMInferenceService endpoints)
				// - Kubernetes clusters often use self-signed or cluster-issued certificates
				// - Authentication is enforced via Bearer token (ServiceAccount token in Authorization header)
				// - Authorization is validated by the model server/gateway, not by this client
				// An attacker capable of MITM within the cluster network would already have sufficient access
				// to compromise the system through other means. This is a trade-off between security and convenience.
				// Potential mitigation: Use the internal gateway Service (ClusterIP) instead of the
				// external route/ingress URL to ensure traffic never leaves the cluster network.
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true, //nolint:gosec // See security context above
				},
				MaxIdleConns:        httpMaxIdleConns,
				MaxIdleConnsPerHost: maxDiscoveryConcurrency, // match goroutine limit
				IdleConnTimeout:     httpIdleConnTimeout,
			},
		},
	}, nil
}

// ListAvailableLLMs discovers and returns models from authorized LLMInferenceServices.
// Addresses are checked in priority order: external gateways first, then internal.
// The returned model URL is the address that was actually accessible.
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
			model := m.discoverModel(ctx, llmIsvc, saToken)
			if model != nil {
				mu.Lock()
				defer mu.Unlock()
				authorizedModels = append(authorizedModels, *model)
			}
			return nil
		})
	}

	_ = g.Wait() // errors handled within goroutines

	return authorizedModels, nil
}

// discoverModel attempts to discover models from an LLMInferenceService by trying
// addresses in priority order (external first, then internal). Returns the first
// successful discovery result, or nil if all addresses fail.
func (m *Manager) discoverModel(ctx context.Context, llmIsvc *kservev1alpha1.LLMInferenceService, saToken string) *Model {
	addresses := m.getPrioritizedAddresses(llmIsvc)
	if len(addresses) == 0 {
		m.logger.Debug("No addresses available for LLMInferenceService",
			"namespace", llmIsvc.Namespace,
			"name", llmIsvc.Name,
		)
		return nil
	}

	llmIsvcMetadata := m.extractMetadata(llmIsvc)

	for i := range addresses {
		addr := &addresses[i]
		if addr.URL == nil {
			continue
		}

		// Use top-level, advertised address for discovery
		llmIsvcMetadata.URL = addr.URL

		modelsEndpoint, err := url.JoinPath(addr.URL.String(), "/v1/models")
		if err != nil {
			m.logger.Debug("Failed to create endpoint URL",
				"namespace", llmIsvc.Namespace,
				"name", llmIsvc.Name,
				"address", addr.URL.String(),
				"error", err,
			)
			continue
		}
		llmIsvcMetadata.ModelsEndpoint = modelsEndpoint

		discoveredModels := m.fetchModelsWithRetry(ctx, saToken, llmIsvcMetadata)
		if discoveredModels != nil {
			m.logger.Debug("Successfully discovered models via address",
				"namespace", llmIsvc.Namespace,
				"name", llmIsvc.Name,
				"address", addr.URL.String(),
				"modelCount", len(discoveredModels),
			)
			return m.enrichModel(discoveredModels, llmIsvcMetadata)
		}

		m.logger.Debug("Address failed, trying next",
			"namespace", llmIsvc.Namespace,
			"name", llmIsvc.Name,
			"address", addr.URL.String(),
		)
	}

	return nil
}

func (m *Manager) extractMetadata(llmIsvc *kservev1alpha1.LLMInferenceService) llmInferenceServiceMetadata {
	return llmInferenceServiceMetadata{
		ServiceName: llmIsvc.Name,
		ModelName:   m.extractModelName(llmIsvc),
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

// getPrioritizedAddresses returns addresses sorted by priority: external > internal > others.
// status.URL is always appended last as a best-effort fallback.
func (m *Manager) getPrioritizedAddresses(llmIsvc *kservev1alpha1.LLMInferenceService) []duckv1.Addressable {
	var addresses []duckv1.Addressable

	addresses = append(addresses, llmIsvc.Status.Addresses...)

	if len(addresses) == 0 && llmIsvc.Status.Address != nil && llmIsvc.Status.Address.URL != nil {
		addresses = append(addresses, *llmIsvc.Status.Address)
	}

	slices.SortStableFunc(addresses, func(a, b duckv1.Addressable) int {
		return addressPriority(a) - addressPriority(b)
	})

	if llmIsvc.Status.URL != nil {
		addresses = append(addresses, duckv1.Addressable{URL: llmIsvc.Status.URL})
	}

	return addresses
}

func addressPriority(addr duckv1.Addressable) int {
	name := ""
	if addr.Name != nil {
		name = strings.ToLower(*addr.Name)
	}

	switch {
	case strings.Contains(name, "external"):
		return 0
	case strings.Contains(name, "internal"):
		return 1
	default:
		return 2
	}
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

func (m *Manager) extractModelDetails(llmIsvc *kservev1alpha1.LLMInferenceService) *Details {
	annotations := llmIsvc.GetAnnotations()
	if annotations == nil {
		return nil
	}

	genaiUseCase := annotations[constant.AnnotationGenAIUseCase]
	description := annotations[constant.AnnotationDescription]
	displayName := annotations[constant.AnnotationDisplayName]

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
