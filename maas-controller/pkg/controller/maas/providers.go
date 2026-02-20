/*
Copyright 2025.

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

package maas

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// ErrKindNotImplemented indicates the model kind is recognized but not implemented (e.g. ExternalModel stub).
var ErrKindNotImplemented = errors.New("model kind not implemented")

// RouteResolver returns the HTTPRoute name and namespace for a MaaSModel.
// Used by findHTTPRouteForModel and by AuthPolicy/Subscription controllers to attach policies.
type RouteResolver interface {
	HTTPRouteForModel(ctx context.Context, c client.Reader, model *maasv1alpha1.MaaSModel) (routeName, routeNamespace string, err error)
}

// BackendHandler encapsulates kind-specific behavior for a MaaSModel.
// The MaaSModel reconciler calls these in order; it does not switch on kind.
type BackendHandler interface {
	// ReconcileRoute creates or updates the HTTPRoute for this model, or validates it exists (e.g. llmisvc).
	ReconcileRoute(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModel) error
	// Status returns the endpoint URL and whether the model is ready (phase Ready).
	Status(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModel) (endpoint string, ready bool, err error)
	// GetModelEndpoint returns the endpoint URL for the model. Kind-specific: e.g. llmisvc uses gateway/HTTPRoute
	// hostname + path; ExternalModel (when implemented) would use its own logic and need not follow the same path assumptions.
	GetModelEndpoint(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModel) (string, error)
	// CleanupOnDelete is called when the MaaSModel is deleted (e.g. delete HTTPRoute for ExternalModel).
	CleanupOnDelete(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModel) error
}

// backendHandlerFactory creates a BackendHandler that uses the given reconciler for client/scheme and shared helpers.
type backendHandlerFactory func(*MaaSModelReconciler) BackendHandler

// routeResolverFactory creates a RouteResolver. RouteResolvers are stateless and only need a client.Reader at call time,
// so we pass the reader in HTTPRouteForModel; the factory can return a stateless resolver per kind.
type routeResolverFactory func() RouteResolver

var (
	backendHandlerFactories = map[string]backendHandlerFactory{}
	routeResolverFactories  = map[string]routeResolverFactory{}
)

func init() {
	// CRD enum is LLMInferenceService;ExternalModel (see api/maas/v1alpha1/maasmodel_types.go). Register both.
	backendHandlerFactories["LLMInferenceService"] = func(r *MaaSModelReconciler) BackendHandler { return &llmisvcHandler{r} }
	backendHandlerFactories["llmisvc"] = func(r *MaaSModelReconciler) BackendHandler { return &llmisvcHandler{r} } // alias for backwards compatibility
	backendHandlerFactories["ExternalModel"] = func(r *MaaSModelReconciler) BackendHandler { return &externalModelHandler{r} }

	routeResolverFactories["LLMInferenceService"] = func() RouteResolver { return &llmisvcRouteResolver{} }
	routeResolverFactories["llmisvc"] = func() RouteResolver { return &llmisvcRouteResolver{} }
	routeResolverFactories["ExternalModel"] = func() RouteResolver { return &externalModelRouteResolver{} }
}

// GetBackendHandler returns the BackendHandler for the given kind, or nil if unknown.
func GetBackendHandler(kind string, r *MaaSModelReconciler) BackendHandler {
	f := backendHandlerFactories[kind]
	if f == nil {
		return nil
	}
	return f(r)
}

// GetRouteResolver returns the RouteResolver for the given kind, or nil if unknown.
func GetRouteResolver(kind string) RouteResolver {
	f := routeResolverFactories[kind]
	if f == nil {
		return nil
	}
	return f()
}

// ErrModelNotFound is returned when a MaaSModel cannot be found by name (e.g. in findHTTPRouteForModel).
var ErrModelNotFound = errors.New("MaaSModel not found")

// findHTTPRouteForModel finds the MaaSModel by name, uses the kind's RouteResolver to get HTTPRoute name/namespace,
// and verifies the HTTPRoute exists. Returns (httpRouteName, httpRouteNamespace, error).
func findHTTPRouteForModel(ctx context.Context, c client.Reader, defaultNS, modelName string) (string, string, error) {
	maasModelList := &maasv1alpha1.MaaSModelList{}
	if err := c.List(ctx, maasModelList); err != nil {
		return "", "", fmt.Errorf("failed to list MaaSModels: %w", err)
	}

	var maasModel *maasv1alpha1.MaaSModel
	for i := range maasModelList.Items {
		if maasModelList.Items[i].Name != modelName {
			continue
		}
		if !maasModelList.Items[i].GetDeletionTimestamp().IsZero() {
			continue
		}
		if maasModelList.Items[i].Namespace == defaultNS {
			maasModel = &maasModelList.Items[i]
			break
		}
		if maasModel == nil {
			maasModel = &maasModelList.Items[i]
		}
	}

	if maasModel == nil {
		return "", "", fmt.Errorf("%w: %s", ErrModelNotFound, modelName)
	}

	resolver := GetRouteResolver(maasModel.Spec.ModelRef.Kind)
	if resolver == nil {
		return "", "", fmt.Errorf("unknown model kind: %s", maasModel.Spec.ModelRef.Kind)
	}

	routeName, routeNS, err := resolver.HTTPRouteForModel(ctx, c, maasModel)
	if err != nil {
		return "", "", err
	}

	// Verify HTTPRoute exists
	if _, err := getHTTPRoute(ctx, c, routeName, routeNS); err != nil {
		return "", "", err
	}
	return routeName, routeNS, nil
}

func getHTTPRoute(ctx context.Context, c client.Reader, name, ns string) (*gatewayapiv1.HTTPRoute, error) {
	route := &gatewayapiv1.HTTPRoute{}
	err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, route)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("HTTPRoute %s/%s not found for model", ns, name)
		}
		return nil, fmt.Errorf("failed to get HTTPRoute %s/%s: %w", ns, name, err)
	}
	return route, nil
}
