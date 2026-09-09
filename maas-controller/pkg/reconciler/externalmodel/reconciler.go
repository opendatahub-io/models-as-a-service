package externalmodel

import (
	"context"
	"fmt"
	"strconv"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/modelnaming"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

const (
	// annotationPort overrides the default port (443).
	annotationPort = "maas.opendatahub.io/port"

	// annotationTLS controls TLS origination (default "true").
	annotationTLS = "maas.opendatahub.io/tls"
)

var inferenceExternalModelGVK = schema.GroupVersionKind{
	Group:   "inference.opendatahub.io",
	Version: "v1alpha1",
	Kind:    "ExternalModel",
}

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
	return r.GatewayName
}

func (r *Reconciler) gatewayNamespace() string {
	return r.GatewayNamespace
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
//+kubebuilder:rbac:groups=maas.opendatahub.io,resources=externalmodels/status,verbs=get;update;patch
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

	// If an inference.opendatahub.io ExternalModel exists for this model,
	// the IPP controller owns networking. Tear down legacy maas-* children.
	if superseded, err := r.isSupersededByInference(ctx, req.NamespacedName); err != nil {
		return ctrl.Result{}, err
	} else if superseded {
		log.FromContext(ctx).Info("inference ExternalModel exists, tearing down legacy networking",
			"name", req.Name, "namespace", req.Namespace)
		if _, err := r.teardownLegacyChildren(ctx, extModel); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.markMigrated(ctx, extModel)
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
	resourceName := modelnaming.ExternalModelResourceName(name)
	gwName := r.gatewayName()
	gwNamespace := r.gatewayNamespace()
	labels := commonLabels(name)

	// 1. ExternalName Service (backend for HTTPRoute)
	svc := buildService(extModel.Spec.Endpoint, resourceName, ns, port, labels)
	if err := controllerutil.SetControllerReference(extModel, svc, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set owner on Service: %w", err)
	}
	if err := r.applyService(ctx, logger, svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create Service: %w", err)
	}

	// 2. ServiceEntry (registers external host in mesh)
	se := buildServiceEntry(extModel.Spec.Endpoint, resourceName, ns, port, tls, labels)
	if err := r.setUnstructuredOwner(extModel, se); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set owner on ServiceEntry: %w", err)
	}
	if err := r.applyUnstructured(ctx, logger, se); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create ServiceEntry: %w", err)
	}

	// 3. DestinationRule (only if TLS; delete stale DR when TLS is disabled)
	if tls {
		dr := buildDestinationRule(extModel.Spec.Endpoint, resourceName, ns, labels)
		if err := r.setUnstructuredOwner(extModel, dr); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to set owner on DestinationRule: %w", err)
		}
		if err := r.applyUnstructured(ctx, logger, dr); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create DestinationRule: %w", err)
		}
	} else {
		if err := r.deleteIfExists(ctx, logger, "DestinationRule", resourceName, ns, schema.GroupVersionKind{
			Group: "networking.istio.io", Version: "v1", Kind: "DestinationRule",
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to delete stale DestinationRule: %w", err)
		}
	}

	// 4. HTTPRoute (routes requests to external provider via gateway)
	hr := buildHTTPRoute(extModel.Spec.Endpoint, resourceName, resourceName, name, extModel.Spec.TargetModel, ns, port, gwName, gwNamespace, labels)
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
	if !isManaged(obj) {
		log.Info("Resource opted out of management, skipping deletion", "kind", kind, "name", name, "namespace", namespace)
		return nil
	}
	log.Info("Deleting resource", "kind", kind, "name", name)
	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete %s %s/%s: %w", kind, namespace, name, err)
	}
	return nil
}

// isManaged reports whether obj has opted into maas/opendatahub controller management.
// When the annotation is absent or any value other than "false", the resource is managed.
// Only an explicit opendatahub.io/managed=false opts the resource out.
func isManaged(obj metav1.Object) bool {
	val, ok := obj.GetAnnotations()[tenantreconcile.AnnotationManaged]
	if !ok {
		return true
	}
	return val != "false"
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
	if !isManaged(existing) {
		log.Info("Service opted out of management, skipping update", "name", existing.Name, "namespace", existing.Namespace)
		return nil
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
	if !isManaged(existing) {
		log.Info("Resource opted out of management, skipping update", "kind", existing.GetKind(), "name", existing.GetName(), "namespace", existing.GetNamespace())
		return nil
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
	if !isManaged(existing) {
		log.Info("HTTPRoute opted out of management, skipping update", "name", existing.Name, "namespace", existing.Namespace)
		return nil
	}
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	existing.OwnerReferences = desired.OwnerReferences
	log.Info("Updating HTTPRoute", "name", desired.Name)
	return r.Update(ctx, existing)
}

// isSupersededByInference reports whether an inference.opendatahub.io ExternalModel
// exists for the same name/namespace AND has published its HTTPRoute
// (status.httpRouteName is set), meaning the IPP controller now owns networking.
//
// Teardown is gated on that readiness signal, not on mere existence: if we removed
// the legacy maas-* route the moment the inference CR appeared — before its route is
// programmed — there would be a window with no route for the model, causing a
// routing/auth gap during the migration cutover. The MaaSModelRef handler waits for
// the same signal (see providers_external.go), so this keeps the two in lockstep.
//
// When the inference CRD is not installed (IsNoMatchError), the legacy reconciler
// proceeds normally, so pure 3.4 clusters are unaffected.
func (r *Reconciler) isSupersededByInference(ctx context.Context, key types.NamespacedName) (bool, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(inferenceExternalModelGVK)
	if err := r.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check inference ExternalModel %s/%s: %w", key.Namespace, key.Name, err)
	}
	routeName, _, err := unstructured.NestedString(obj.Object, "status", "httpRouteName")
	if err != nil {
		return false, fmt.Errorf("failed to read status.httpRouteName on inference ExternalModel %s/%s: %w", key.Namespace, key.Name, err)
	}
	return routeName != "", nil
}

// teardownLegacyChildren deletes the maas-* networking resources (Service,
// ServiceEntry, DestinationRule, HTTPRoute) that were created by this reconciler
// for the given ExternalModel, then returns a no-requeue result.
func (r *Reconciler) teardownLegacyChildren(ctx context.Context, extModel *maasv1alpha1.ExternalModel) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	resourceName := modelnaming.ExternalModelResourceName(extModel.Name)
	ns := extModel.Namespace

	if err := r.deleteIfExists(ctx, logger, "HTTPRoute", resourceName, ns, schema.GroupVersionKind{
		Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute",
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to delete legacy HTTPRoute: %w", err)
	}
	if err := r.deleteIfExists(ctx, logger, "DestinationRule", resourceName, ns, schema.GroupVersionKind{
		Group: "networking.istio.io", Version: "v1", Kind: "DestinationRule",
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to delete legacy DestinationRule: %w", err)
	}
	if err := r.deleteIfExists(ctx, logger, "ServiceEntry", resourceName, ns, schema.GroupVersionKind{
		Group: "networking.istio.io", Version: "v1", Kind: "ServiceEntry",
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to delete legacy ServiceEntry: %w", err)
	}

	svc := &corev1.Service{}
	svcKey := types.NamespacedName{Name: resourceName, Namespace: ns}
	if err := r.Get(ctx, svcKey, svc); err == nil {
		if isManaged(svc) {
			logger.Info("Deleting legacy Service", "name", resourceName)
			if err := r.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("failed to delete legacy Service: %w", err)
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("failed to get legacy Service: %w", err)
	}

	return ctrl.Result{}, nil
}

// markMigrated records that the inference API now owns networking for this
// legacy ExternalModel. The legacy object remains as the owner of the migrated
// inference resources, so the status gives users a durable migration signal.
func (r *Reconciler) markMigrated(ctx context.Context, extModel *maasv1alpha1.ExternalModel) error {
	desiredMessage := fmt.Sprintf("Model migrated to inference.opendatahub.io ExternalModel: %s", extModel.Name)
	if extModel.Status.Phase == "Migrated" && extModel.Status.Message == desiredMessage {
		return nil
	}

	extModel.Status.Phase = "Migrated"
	extModel.Status.Message = desiredMessage
	if err := r.Status().Update(ctx, extModel); err != nil {
		return fmt.Errorf("failed to mark legacy ExternalModel %s/%s as migrated: %w", extModel.Namespace, extModel.Name, err)
	}
	return nil
}

// SetupWithManager registers the reconciler to watch ExternalModel CRs.
//
// It additionally watches inference.opendatahub.io ExternalModels so that when the
// legacy-migration controller creates the canonical CR, the matching legacy
// ExternalModel is re-reconciled promptly and its maas-* networking is torn down —
// rather than waiting for the periodic resync (which can be hours). Each inference
// event is mapped to the same name/namespace; Reconcile then no-ops if no legacy
// ExternalModel exists there, so native inference-only models are never affected.
//
// The watch is only wired when the inference CRD is installed. On pure 3.4 clusters
// the CRD is absent and establishing an informer for it would fail cache sync, so we
// skip it; there is no inference CR to react to on those clusters anyway.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&maasv1alpha1.ExternalModel{}).
		Named("external-model-reconciler")

	if _, err := mgr.GetRESTMapper().RESTMapping(
		inferenceExternalModelGVK.GroupKind(), inferenceExternalModelGVK.Version,
	); err == nil {
		inferenceEM := &unstructured.Unstructured{}
		inferenceEM.SetGroupVersionKind(inferenceExternalModelGVK)
		b = b.Watches(inferenceEM, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, obj client.Object) []reconcile.Request {
				return []reconcile.Request{{
					NamespacedName: types.NamespacedName{
						Name:      obj.GetName(),
						Namespace: obj.GetNamespace(),
					},
				}}
			},
		))
	} else {
		mgr.GetLogger().Info("inference.opendatahub.io ExternalModel CRD not installed; "+
			"skipping supersede watch (legacy ExternalModel reconciler runs unchanged)",
			"gvk", inferenceExternalModelGVK.String())
	}

	return b.Complete(r)
}
