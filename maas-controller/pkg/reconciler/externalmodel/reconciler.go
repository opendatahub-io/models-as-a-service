package externalmodel

import (
	"context"
	"fmt"
	"strconv"

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
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

const (
	// annotationPort overrides the default port (443).
	annotationPort = "maas.opendatahub.io/port"

	// annotationTLS controls TLS origination (default "true").
	annotationTLS = "maas.opendatahub.io/tls"

	defaultGatewayName      = tenantreconcile.DefaultGatewayName
	defaultGatewayNamespace = tenantreconcile.DefaultGatewayNamespace
)

// Reconciler watches ExternalModel CRs and creates the Istio resources
// needed to route to the external provider.
//
// All resources are created in the ExternalModel's namespace.
// OwnerReferences on each resource ensure Kubernetes garbage collection
// handles cleanup when the ExternalModel is deleted — no finalizer needed.
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

// commonLabels returns labels applied to all managed resources.
func commonLabels(modelName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by":       "maas-external-model-reconciler",
		"maas.opendatahub.io/external-model": modelName,
	}
}

// getTLSInfo reads optional TLS overrides from ExternalModel annotations.
// Returns tls enabled (default true) and port (default 443).
func getTLSInfo(extModel *maasv1alpha1.ExternalModel) (tls bool, port int32, err error) {
	tls = true
	port = 443

	annotations := extModel.GetAnnotations()
	if annotations == nil {
		return
	}

	if portStr, ok := annotations[annotationPort]; ok {
		p, parseErr := strconv.ParseInt(portStr, 10, 32)
		if parseErr != nil {
			return false, 0, fmt.Errorf("invalid port %q: %w", portStr, parseErr)
		}
		if p < 1 || p > 65535 {
			return false, 0, fmt.Errorf("port %d out of range (1-65535)", p)
		}
		port = int32(p)
	}

	if tlsStr, ok := annotations[annotationTLS]; ok {
		parsed, parseErr := strconv.ParseBool(tlsStr)
		if parseErr != nil {
			return false, 0, fmt.Errorf("invalid tls value %q: %w", tlsStr, parseErr)
		}
		tls = parsed
	}

	return
}

//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=externalmodels,verbs=get;list;watch
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=externalmodels/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;delete
//+kubebuilder:rbac:groups=networking.istio.io,resources=serviceentries,verbs=get;list;watch;create;update
//+kubebuilder:rbac:groups=networking.istio.io,resources=destinationrules,verbs=get;list;watch;create;update;delete

// Reconcile handles create/update/delete of ExternalModel CRs.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log.FromContext(ctx).Info("Reconciling ExternalModel", "namespace", req.Namespace, "name", req.Name)

	extModel := &maasv1alpha1.ExternalModel{}
	if err := r.Get(ctx, req.NamespacedName, extModel); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Nothing to do on deletion — OwnerReferences handle cleanup
	if !extModel.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}

	tls, port, err := getTLSInfo(extModel)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("invalid ExternalModel annotations: %w", err)
	}

	logger := r.Log.WithValues("externalmodel", req.NamespacedName)
	logger.Info("Reconciling ExternalModel",
		"provider", extModel.Spec.Provider,
		"endpoint", extModel.Spec.Endpoint,
		"tls", tls,
	)

	ns := extModel.Namespace
	name := extModel.Name
	gwName := r.gatewayName()
	gwNamespace := r.gatewayNamespace()
	labels := commonLabels(name)

	// 1. ExternalName Service (backend for HTTPRoute)
	svc := buildService(extModel.Spec.Endpoint, name, ns, port, labels)
	if err := controllerutil.SetControllerReference(extModel, svc, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set owner on Service: %w", err)
	}
	if err := r.applyService(ctx, logger, svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create Service: %w", err)
	}

	// 2. ServiceEntry (registers external host in mesh)
	se := buildServiceEntry(extModel.Spec.Endpoint, name, ns, port, tls, labels)
	if err := r.setUnstructuredOwner(extModel, se); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set owner on ServiceEntry: %w", err)
	}
	if err := r.applyUnstructured(ctx, logger, se); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create ServiceEntry: %w", err)
	}

	// 3. DestinationRule (only if TLS; delete stale DR when TLS is disabled)
	if tls {
		dr := buildDestinationRule(extModel.Spec.Endpoint, name, ns, labels)
		if err := r.setUnstructuredOwner(extModel, dr); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to set owner on DestinationRule: %w", err)
		}
		if err := r.applyUnstructured(ctx, logger, dr); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create DestinationRule: %w", err)
		}
	} else {
		if err := r.deleteIfExists(ctx, logger, "DestinationRule", name, ns, schema.GroupVersionKind{
			Group: "networking.istio.io", Version: "v1", Kind: "DestinationRule",
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to delete stale DestinationRule: %w", err)
		}
	}

	// 4. HTTPRoute (routes requests to external provider via gateway)
	hr := buildHTTPRoute(extModel.Spec.Endpoint, name, extModel.Spec.TargetModel, ns, port, gwName, gwNamespace, labels)
	if err := controllerutil.SetControllerReference(extModel, hr, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set owner on HTTPRoute: %w", err)
	}
	if err := r.applyHTTPRoute(ctx, logger, hr); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create HTTPRoute: %w", err)
	}

	logger.Info("ExternalModel resources reconciled successfully",
		"service", svc.Name,
		"serviceEntry", se.GetName(),
		"httpRoute", hr.Name,
		"namespace", ns,
	)

	return ctrl.Result{}, nil
}

// setUnstructuredOwner sets the controller OwnerReference on an unstructured resource.
func (r *Reconciler) setUnstructuredOwner(owner *maasv1alpha1.ExternalModel, obj *unstructured.Unstructured) error {
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
	specChanged := !equality.Semantic.DeepEqual(existing.Spec, desired.Spec)
	ownerChanged := !equality.Semantic.DeepEqual(existing.OwnerReferences, desired.OwnerReferences)
	labelsChanged := !equality.Semantic.DeepEqual(existing.Labels, desired.Labels)
	if specChanged || ownerChanged || labelsChanged {
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

// SetupWithManager registers the reconciler to watch ExternalModel CRs.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&maasv1alpha1.ExternalModel{}).
		Named("external-model-reconciler").
		Complete(r)
}
