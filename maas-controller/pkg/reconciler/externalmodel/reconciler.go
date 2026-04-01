package externalmodel

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

const (
	// AnnExtraHeaders allows setting additional headers on the HTTPRoute.
	// Format: "key1=value1,key2=value2"
	AnnExtraHeaders = "maas.opendatahub.io/extra-headers"

	// AnnPort overrides the default port (443).
	AnnPort = "maas.opendatahub.io/port"

	// AnnTLS controls TLS origination (default "true").
	AnnTLS = "maas.opendatahub.io/tls"

	// AnnPathPrefix overrides the default path prefix (/external/<provider>/).
	AnnPathPrefix = "maas.opendatahub.io/path-prefix"

	// Default gateway (matches MaaS controller defaults)
	defaultGatewayName      = "maas-default-gateway"
	defaultGatewayNamespace = "openshift-ingress"
)

// Reconciler watches MaaSModelRef CRs with kind=ExternalModel and creates
// the Istio resources needed to route to the external provider.
//
// All resources are created in the model's namespace (same as the MaaSModelRef).
// OwnerReferences on each resource ensure Kubernetes garbage collection handles
// cleanup when the MaaSModelRef is deleted — no finalizer needed.
type Reconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Log              logr.Logger
	GatewayName      string
	GatewayNamespace string
}

func (r *Reconciler) gatewayName() string {
	if r.GatewayName != "" {
		return r.GatewayName
	}
	return defaultGatewayName
}

func (r *Reconciler) gatewayNamespace() string {
	if r.GatewayNamespace != "" {
		return r.GatewayNamespace
	}
	return defaultGatewayNamespace
}

// Reconcile handles create/update/delete of MaaSModelRef CRs with kind=ExternalModel.
// The ExternalModel kind filter is handled by the predicate in SetupWithManager.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("maasmodelref", req.NamespacedName)

	model := &maasv1alpha1.MaaSModelRef{}
	if err := r.Get(ctx, req.NamespacedName, model); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Nothing to do on deletion — OwnerReferences handle cleanup
	if !model.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}

	// Fetch the referenced ExternalModel CR to get provider configuration
	extModel := &maasv1alpha1.ExternalModel{}
	extModelKey := types.NamespacedName{
		Name:      model.Spec.ModelRef.Name,
		Namespace: model.Namespace,
	}
	if err := r.Get(ctx, extModelKey, extModel); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("ExternalModel CR not found, waiting", "name", model.Spec.ModelRef.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get ExternalModel %s: %w", model.Spec.ModelRef.Name, err)
	}

	spec, err := specFromExternalModel(extModel, model)
	if err != nil {
		log.Error(err, "Failed to parse ExternalModel spec")
		return ctrl.Result{}, fmt.Errorf("invalid ExternalModel spec: %w", err)
	}

	log.Info("Reconciling ExternalModel",
		"provider", spec.Provider,
		"endpoint", spec.Endpoint,
		"tls", spec.TLS,
	)

	ns := model.Namespace
	gwName := r.gatewayName()
	gwNamespace := r.gatewayNamespace()
	labels := commonLabels(model.GetName())

	// 1. ExternalName Service (backend for HTTPRoute)
	svc := BuildService(spec, model.Name, ns, labels)
	if err := controllerutil.SetControllerReference(model, svc, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set owner on Service: %w", err)
	}
	if err := r.applyService(ctx, log, svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create Service: %w", err)
	}

	// 2. ServiceEntry (registers external host in mesh)
	se := BuildServiceEntry(spec, model.Name, ns, labels)
	if err := r.setUnstructuredOwner(model, se); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set owner on ServiceEntry: %w", err)
	}
	if err := r.applyUnstructured(ctx, log, se); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create ServiceEntry: %w", err)
	}

	// 3. DestinationRule (only if TLS; delete stale DR when TLS is disabled)
	drName := ModelDestinationRuleName(model.Name)
	if spec.TLS {
		dr := BuildDestinationRule(spec, model.Name, ns, labels)
		if err := r.setUnstructuredOwner(model, dr); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to set owner on DestinationRule: %w", err)
		}
		if err := r.applyUnstructured(ctx, log, dr); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create DestinationRule: %w", err)
		}
	} else {
		if err := r.deleteIfExists(ctx, log, "DestinationRule", drName, ns, schema.GroupVersionKind{
			Group: "networking.istio.io", Version: "v1", Kind: "DestinationRule",
		}); err != nil {
			log.Error(err, "Failed to delete stale DestinationRule", "name", drName)
		}
	}

	// 4. HTTPRoute (routes requests to external provider via gateway)
	hr := BuildHTTPRoute(spec, model.Name, ns, gwName, gwNamespace, labels)
	if err := controllerutil.SetControllerReference(model, hr, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set owner on HTTPRoute: %w", err)
	}
	if err := r.applyHTTPRoute(ctx, log, hr); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create HTTPRoute: %w", err)
	}

	log.Info("ExternalModel resources reconciled successfully",
		"service", svc.Name,
		"serviceEntry", se.GetName(),
		"httpRoute", hr.Name,
		"namespace", ns,
	)

	return ctrl.Result{}, nil
}

// setUnstructuredOwner sets the controller OwnerReference on an unstructured resource.
func (r *Reconciler) setUnstructuredOwner(owner *maasv1alpha1.MaaSModelRef, obj *unstructured.Unstructured) error {
	isController := true
	blockDeletion := true
	obj.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion:         owner.APIVersion,
			Kind:               owner.Kind,
			Name:               owner.Name,
			UID:                owner.UID,
			Controller:         &isController,
			BlockOwnerDeletion: &blockDeletion,
		},
	})
	return nil
}

// deleteIfExists deletes an unstructured resource if it exists.
func (r *Reconciler) deleteIfExists(ctx context.Context, log logr.Logger, kind, name, namespace string, gvk schema.GroupVersionKind) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get %s %s/%s: %w", kind, namespace, name, err)
	}
	log.Info("Deleting resource", "kind", kind, "name", name)
	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete %s %s/%s: %w", kind, namespace, name, err)
	}
	return nil
}

// applyService creates or updates a Service.
func (r *Reconciler) applyService(ctx context.Context, log logr.Logger, desired *corev1.Service) error {
	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		log.Info("Creating Service", "name", desired.Name)
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
		existing.Spec = desired.Spec
		existing.Labels = desired.Labels
		existing.OwnerReferences = desired.OwnerReferences
		log.Info("Updating Service", "name", desired.Name)
		return r.Update(ctx, existing)
	}
	return nil
}

// applyUnstructured creates or updates an unstructured resource (ServiceEntry, DestinationRule).
func (r *Reconciler) applyUnstructured(ctx context.Context, log logr.Logger, desired *unstructured.Unstructured) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(desired.GroupVersionKind())
	err := r.Get(ctx, types.NamespacedName{Name: desired.GetName(), Namespace: desired.GetNamespace()}, existing)
	if apierrors.IsNotFound(err) {
		log.Info("Creating resource", "kind", desired.GetKind(), "name", desired.GetName())
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	desired.SetResourceVersion(existing.GetResourceVersion())
	log.Info("Updating resource", "kind", desired.GetKind(), "name", desired.GetName())
	return r.Update(ctx, desired)
}

// applyHTTPRoute creates or updates an HTTPRoute.
func (r *Reconciler) applyHTTPRoute(ctx context.Context, log logr.Logger, desired *gatewayapiv1.HTTPRoute) error {
	existing := &gatewayapiv1.HTTPRoute{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		log.Info("Creating HTTPRoute", "name", desired.Name)
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	existing.OwnerReferences = desired.OwnerReferences
	log.Info("Updating HTTPRoute", "name", desired.Name)
	return r.Update(ctx, existing)
}

// specFromExternalModel reads ExternalModelSpec from the ExternalModel CR and
// optional annotation overrides from the MaaSModelRef.
// Provider and endpoint come from the ExternalModel CR (PR #586).
// Port, TLS, path-prefix, and extra-headers are optional annotation overrides on the MaaSModelRef.
func specFromExternalModel(extModel *maasv1alpha1.ExternalModel, model *maasv1alpha1.MaaSModelRef) (ExternalModelSpec, error) {
	ann := model.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}

	spec := ExternalModelSpec{
		Provider:   extModel.Spec.Provider,
		Endpoint:   extModel.Spec.Endpoint,
		PathPrefix: ann[AnnPathPrefix],
		TLS:        true,
		Port:       443,
		// TLSInsecureSkipVerify: extModel.Spec.TLSInsecureSkipVerify, // requires issue #627 CRD change
	}

	if spec.Provider == "" {
		return spec, fmt.Errorf("provider is required on ExternalModel %s", extModel.Name)
	}
	if spec.Endpoint == "" {
		return spec, fmt.Errorf("endpoint is required on ExternalModel %s", extModel.Name)
	}

	if portStr, ok := ann[AnnPort]; ok {
		p, err := strconv.ParseInt(portStr, 10, 32)
		if err != nil {
			return spec, fmt.Errorf("invalid port %q: %w", portStr, err)
		}
		if p < 1 || p > 65535 {
			return spec, fmt.Errorf("port %d out of range (1-65535)", p)
		}
		spec.Port = int32(p)
	}

	if tlsStr, ok := ann[AnnTLS]; ok {
		parsed, err := strconv.ParseBool(tlsStr)
		if err != nil {
			return spec, fmt.Errorf("invalid tls value %q: %w", tlsStr, err)
		}
		spec.TLS = parsed
	}

	if extraStr, ok := ann[AnnExtraHeaders]; ok && extraStr != "" {
		spec.ExtraHeaders = map[string]string{}
		for pair := range strings.SplitSeq(extraStr, ",") {
			kv := strings.SplitN(pair, "=", 2)
			if len(kv) == 2 {
				spec.ExtraHeaders[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
			}
		}
	}

	return spec, nil
}

// externalModelPredicate filters MaaSModelRef events to only ExternalModel kind.
func externalModelPredicate() predicate.Predicate {
	isExternalModel := func(obj client.Object) bool {
		model, ok := obj.(*maasv1alpha1.MaaSModelRef)
		if !ok {
			return false
		}
		return model.Spec.ModelRef.Kind == "ExternalModel"
	}
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return isExternalModel(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return isExternalModel(e.ObjectOld) || isExternalModel(e.ObjectNew)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return isExternalModel(e.Object)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return isExternalModel(e.Object)
		},
	}
}

// SetupWithManager registers the reconciler to watch MaaSModelRef CRs
// with kind=ExternalModel only (filtered by predicate).
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&maasv1alpha1.MaaSModelRef{}).
		WithEventFilter(externalModelPredicate()).
		Named("external-model-reconciler").
		Complete(r)
}
