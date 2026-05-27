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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

const (
	aigatewayFinalizer = "maas.opendatahub.io/aigateway-cleanup"

	aigatewayManagedLabel = "maas.opendatahub.io/managed-by-aigateway"

	aigatewayNameAnnotation      = "maas.opendatahub.io/aigateway-name"
	aigatewayNamespaceAnnotation = "maas.opendatahub.io/aigateway-namespace"
	aigatewayCreatedAnnotation   = "maas.opendatahub.io/created-by-aigateway"

	aigatewayTenantAdminRoleSuffix = "tenant-admin"
	aigatewayAccessRoleSuffix      = "object-admin"
)

// AIGatewayReconciler reconciles AIGateway tenant bootstrap resources.
type AIGatewayReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader is used for reads that must bypass the Tenant namespace cache scope.
	APIReader client.Reader

	// AppNamespace is the protected ODH application namespace. AIGateway objects
	// and tenant namespaces must not live there.
	AppNamespace string
	// TenantNamespace is the legacy default MaaS tenant namespace. AIGateway objects
	// must stay in a separate infra namespace.
	TenantNamespace string
	// GatewayNamespace is the fallback Gateway namespace when spec.gateway.namespace is omitted.
	GatewayNamespace string
}

// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=aigateways,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=aigateways/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=aigateways/finalizers,verbs=update
// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=tenants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives AIGateway bootstrap lifecycle.
func (r *AIGatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var aigw maasv1alpha1.AIGateway
	if err := r.Get(ctx, req.NamespacedName, &aigw); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !aigw.DeletionTimestamp.IsZero() {
		return r.reconcileAIGatewayDelete(ctx, &aigw)
	}

	if !controllerutil.ContainsFinalizer(&aigw, aigatewayFinalizer) {
		base := aigw.DeepCopy()
		controllerutil.AddFinalizer(&aigw, aigatewayFinalizer)
		if err := r.Patch(ctx, &aigw, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	statusSnapshot := aigw.Status.DeepCopy()

	if err := r.validateAIGatewayPlacement(&aigw); err != nil {
		setAIGatewayPhase(&aigw, "Failed", "InvalidPlacement", err.Error())
		return ctrl.Result{}, r.updateAIGatewayStatus(ctx, &aigw, statusSnapshot)
	}

	aigw.Status.TenantNamespace = aigw.Spec.TenantNamespace.Name

	if err := r.ensureTenantNamespace(ctx, &aigw); err != nil {
		if errors.Is(err, errTenantNamespaceMissing) {
			setAIGatewayPhase(&aigw, "Pending", "TenantNamespaceMissing", err.Error())
			if err2 := r.updateAIGatewayStatus(ctx, &aigw, statusSnapshot); err2 != nil {
				return ctrl.Result{}, err2
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		setAIGatewayPhase(&aigw, "Failed", "TenantNamespaceFailed", err.Error())
		if err2 := r.updateAIGatewayStatus(ctx, &aigw, statusSnapshot); err2 != nil {
			return ctrl.Result{}, err2
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	gatewayRef, err := r.ensureTenantGateway(ctx, &aigw)
	aigw.Status.GatewayRef = gatewayRef
	if err != nil {
		setAIGatewayPhase(&aigw, "Failed", "GatewayReconcileFailed", err.Error())
		if err2 := r.updateAIGatewayStatus(ctx, &aigw, statusSnapshot); err2 != nil {
			return ctrl.Result{}, err2
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if err := r.ensureTenantConfig(ctx, &aigw, gatewayRef); err != nil {
		setAIGatewayPhase(&aigw, "Failed", "TenantConfigReconcileFailed", err.Error())
		if err2 := r.updateAIGatewayStatus(ctx, &aigw, statusSnapshot); err2 != nil {
			return ctrl.Result{}, err2
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if err := r.ensureTenantAdminRBAC(ctx, &aigw); err != nil {
		setAIGatewayPhase(&aigw, "Failed", "RBACReconcileFailed", err.Error())
		if err2 := r.updateAIGatewayStatus(ctx, &aigw, statusSnapshot); err2 != nil {
			return ctrl.Result{}, err2
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	setAIGatewayPhase(&aigw, "Active", "Reconciled", "AIGateway bootstrap resources are reconciled")
	if err := r.updateAIGatewayStatus(ctx, &aigw, statusSnapshot); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the AIGateway controller.
func (r *AIGatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&maasv1alpha1.AIGateway{}, builder.WithPredicates(
			predicate.Or(predicate.GenerationChangedPredicate{}, predicate.Funcs{UpdateFunc: deletionTimestampSet}),
		)).
		Complete(r)
}

func (r *AIGatewayReconciler) validateAIGatewayPlacement(aigw *maasv1alpha1.AIGateway) error {
	if aigw.Namespace == "" {
		return fmt.Errorf("AIGateway %q must be namespaced", aigw.Name)
	}
	if r.AppNamespace != "" && aigw.Namespace == r.AppNamespace {
		return fmt.Errorf("AIGateway %s/%s must not be created in the protected application namespace %q", aigw.Namespace, aigw.Name, r.AppNamespace)
	}
	if r.TenantNamespace != "" && aigw.Namespace == r.TenantNamespace {
		return fmt.Errorf("AIGateway %s/%s must be created in a separate infra namespace, not the tenant namespace %q", aigw.Namespace, aigw.Name, r.TenantNamespace)
	}
	if aigw.Spec.TenantNamespace.Name == "" {
		return errors.New("spec.tenantNamespace.name is required")
	}
	if aigw.Spec.TenantNamespace.Name == aigw.Namespace {
		return fmt.Errorf("spec.tenantNamespace.name must be different from the AIGateway infra namespace %q", aigw.Namespace)
	}
	if r.AppNamespace != "" && aigw.Spec.TenantNamespace.Name == r.AppNamespace {
		return fmt.Errorf("spec.tenantNamespace.name must not be the protected application namespace %q", r.AppNamespace)
	}
	return nil
}

var errTenantNamespaceMissing = errors.New("tenant namespace missing")

func (r *AIGatewayReconciler) ensureTenantNamespace(ctx context.Context, aigw *maasv1alpha1.AIGateway) error {
	name := aigw.Spec.TenantNamespace.Name
	var ns corev1.Namespace
	err := r.get(ctx, client.ObjectKey{Name: name}, &ns)
	if apierrors.IsNotFound(err) {
		if !boolDefault(aigw.Spec.TenantNamespace.Create, true) {
			return fmt.Errorf("%w: namespace %q does not exist and spec.tenantNamespace.create=false", errTenantNamespaceMissing, name)
		}
		toCreate := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
		}
		applyAIGatewayMetadata(toCreate, aigw)
		setMapValue(&toCreate.Labels, "opendatahub.io/generated-namespace", "true")
		setMapValue(&toCreate.Annotations, aigatewayCreatedAnnotation, "true")
		if err := r.Create(ctx, toCreate); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create tenant namespace %q: %w", name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get tenant namespace %q: %w", name, err)
	}
	if ns.Status.Phase == corev1.NamespaceTerminating {
		return fmt.Errorf("tenant namespace %q is terminating", name)
	}
	if hasAIGatewayOwnerAnnotations(&ns) && !ownedByAIGateway(&ns, aigw) {
		return fmt.Errorf("tenant namespace %q is managed by another AIGateway", name)
	}
	base := ns.DeepCopy()
	applyAIGatewayMetadata(&ns, aigw)
	if equality.Semantic.DeepEqual(base, &ns) {
		return nil
	}
	if err := r.Patch(ctx, &ns, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch tenant namespace %q: %w", name, err)
	}
	return nil
}

func (r *AIGatewayReconciler) ensureTenantGateway(ctx context.Context, aigw *maasv1alpha1.AIGateway) (maasv1alpha1.TenantGatewayRef, error) {
	ref := r.gatewayRefFor(aigw)
	if ref.Namespace == "" {
		return ref, errors.New("spec.gateway.namespace is required when --gateway-namespace is unset")
	}
	if ref.Name == "" {
		return ref, errors.New("spec.gateway.name is required when AIGateway name is empty")
	}

	var existing gatewayapiv1.Gateway
	key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
	err := r.get(ctx, key, &existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return ref, fmt.Errorf("get Gateway %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	if err == nil && hasAIGatewayOwnerAnnotations(&existing) && !ownedByAIGateway(&existing, aigw) {
		return ref, fmt.Errorf("gateway %s/%s is managed by another AIGateway", ref.Namespace, ref.Name)
	}

	if apierrors.IsNotFound(err) {
		gw := &gatewayapiv1.Gateway{
			TypeMeta: metav1.TypeMeta{
				APIVersion: gatewayapiv1.GroupVersion.String(),
				Kind:       "Gateway",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      ref.Name,
				Namespace: ref.Namespace,
			},
		}
		if err := r.mutateGateway(gw, aigw); err != nil {
			return ref, err
		}
		if err := r.Create(ctx, gw); err != nil && !apierrors.IsAlreadyExists(err) {
			return ref, fmt.Errorf("create Gateway %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		return ref, nil
	}

	base := existing.DeepCopy()
	if err := r.mutateGateway(&existing, aigw); err != nil {
		return ref, err
	}
	if equality.Semantic.DeepEqual(base, &existing) {
		return ref, nil
	}
	if err := r.Patch(ctx, &existing, client.MergeFrom(base)); err != nil {
		return ref, fmt.Errorf("patch Gateway %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	return ref, nil
}

func (r *AIGatewayReconciler) mutateGateway(gw *gatewayapiv1.Gateway, aigw *maasv1alpha1.AIGateway) error {
	applyAIGatewayMetadata(gw, aigw)
	className := "openshift-default"
	if aigw.Spec.Gateway != nil && aigw.Spec.Gateway.GatewayClassName != "" {
		className = aigw.Spec.Gateway.GatewayClassName
	}
	gw.Spec.GatewayClassName = gatewayapiv1.ObjectName(className)
	gw.Spec.Listeners = gatewayListenersFor(aigw)
	return nil
}

func (r *AIGatewayReconciler) gatewayRefFor(aigw *maasv1alpha1.AIGateway) maasv1alpha1.TenantGatewayRef {
	ref := maasv1alpha1.TenantGatewayRef{
		Namespace: r.GatewayNamespace,
		Name:      aigw.Name,
	}
	if aigw.Spec.Gateway != nil {
		if aigw.Spec.Gateway.Namespace != "" {
			ref.Namespace = aigw.Spec.Gateway.Namespace
		}
		if aigw.Spec.Gateway.Name != "" {
			ref.Name = aigw.Spec.Gateway.Name
		}
	}
	return ref
}

func gatewayListenersFor(aigw *maasv1alpha1.AIGateway) []gatewayapiv1.Listener {
	fromAll := gatewayapiv1.NamespacesFromAll
	routes := &gatewayapiv1.AllowedRoutes{
		Namespaces: &gatewayapiv1.RouteNamespaces{From: &fromAll},
	}

	if aigw.Spec.Domain == "" {
		return []gatewayapiv1.Listener{{
			Name:          "http",
			Port:          80,
			Protocol:      gatewayapiv1.HTTPProtocolType,
			AllowedRoutes: routes,
		}}
	}

	hostname := gatewayapiv1.Hostname(aigw.Spec.Domain)

	if aigw.Spec.TLS != nil {
		tlsMode := gatewayapiv1.TLSModeTerminate
		return []gatewayapiv1.Listener{{
			Name:          "https",
			Port:          443,
			Protocol:      gatewayapiv1.HTTPSProtocolType,
			Hostname:      &hostname,
			AllowedRoutes: routes,
			TLS: &gatewayapiv1.GatewayTLSConfig{
				Mode: &tlsMode,
				CertificateRefs: []gatewayapiv1.SecretObjectReference{{
					Name: gatewayapiv1.ObjectName(aigw.Spec.TLS.CertificateRef.Name),
				}},
			},
		}}
	}

	return []gatewayapiv1.Listener{{
		Name:          "http",
		Port:          80,
		Protocol:      gatewayapiv1.HTTPProtocolType,
		Hostname:      &hostname,
		AllowedRoutes: routes,
	}}
}

func (r *AIGatewayReconciler) ensureTenantConfig(ctx context.Context, aigw *maasv1alpha1.AIGateway, gatewayRef maasv1alpha1.TenantGatewayRef) error {
	tenant := &maasv1alpha1.Tenant{
		TypeMeta: metav1.TypeMeta{
			APIVersion: maasv1alpha1.GroupVersion.String(),
			Kind:       maasv1alpha1.TenantKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: aigw.Spec.TenantNamespace.Name,
		},
	}
	return r.upsert(ctx, tenant, aigw, func(obj client.Object) error {
		t, ok := obj.(*maasv1alpha1.Tenant)
		if !ok {
			return fmt.Errorf("expected Tenant, got %T", obj)
		}
		applyAIGatewayMetadata(t, aigw)
		t.Spec.GatewayRef = gatewayRef
		t.Spec.ExternalOIDC = aigw.Spec.OIDC
		return nil
	})
}

func (r *AIGatewayReconciler) ensureTenantAdminRBAC(ctx context.Context, aigw *maasv1alpha1.AIGateway) error {
	subjects, err := r.rbacSubjects(aigw)
	if err != nil {
		return err
	}
	if err := r.ensureTenantNamespaceRole(ctx, aigw); err != nil {
		return err
	}
	if err := r.ensureAIGatewayObjectRole(ctx, aigw); err != nil {
		return err
	}

	if len(subjects) == 0 {
		if err := r.deleteOwnedRoleBinding(ctx, aigw, aigw.Spec.TenantNamespace.Name, tenantAdminRoleName(aigw)); err != nil {
			return err
		}
		return r.deleteOwnedRoleBinding(ctx, aigw, aigw.Namespace, aigatewayAccessRoleName(aigw))
	}
	if err := r.ensureRoleBinding(ctx, aigw, aigw.Spec.TenantNamespace.Name, tenantAdminRoleName(aigw), subjects); err != nil {
		return err
	}
	return r.ensureRoleBinding(ctx, aigw, aigw.Namespace, aigatewayAccessRoleName(aigw), subjects)
}

func (r *AIGatewayReconciler) ensureTenantNamespaceRole(ctx context.Context, aigw *maasv1alpha1.AIGateway) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantAdminRoleName(aigw),
			Namespace: aigw.Spec.TenantNamespace.Name,
		},
	}
	return r.upsert(ctx, role, aigw, func(obj client.Object) error {
		role, ok := obj.(*rbacv1.Role)
		if !ok {
			return fmt.Errorf("expected Role, got %T", obj)
		}
		applyAIGatewayMetadata(role, aigw)
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups: []string{maasv1alpha1.GroupVersion.Group},
				Resources: []string{
					"maasauthpolicies",
					"maassubscriptions",
				},
				Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			{
				APIGroups:     []string{maasv1alpha1.GroupVersion.Group},
				Resources:     []string{"tenants"},
				ResourceNames: []string{maasv1alpha1.TenantInstanceName},
				Verbs:         []string{"get", "update", "patch"},
			},
			{
				APIGroups: []string{maasv1alpha1.GroupVersion.Group},
				Resources: []string{
					"maasmodelrefs",
				},
				Verbs: []string{"get", "list", "watch"},
			},
		}
		return nil
	})
}

func (r *AIGatewayReconciler) ensureAIGatewayObjectRole(ctx context.Context, aigw *maasv1alpha1.AIGateway) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      aigatewayAccessRoleName(aigw),
			Namespace: aigw.Namespace,
		},
	}
	return r.upsert(ctx, role, aigw, func(obj client.Object) error {
		role, ok := obj.(*rbacv1.Role)
		if !ok {
			return fmt.Errorf("expected Role, got %T", obj)
		}
		applyAIGatewayMetadata(role, aigw)
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups:     []string{maasv1alpha1.GroupVersion.Group},
				Resources:     []string{"aigateways"},
				ResourceNames: []string{aigw.Name},
				Verbs:         []string{"get", "update", "patch"},
			},
		}
		return nil
	})
}

func (r *AIGatewayReconciler) ensureRoleBinding(ctx context.Context, aigw *maasv1alpha1.AIGateway, namespace, name string, subjects []rbacv1.Subject) error {
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	return r.upsert(ctx, binding, aigw, func(obj client.Object) error {
		binding, ok := obj.(*rbacv1.RoleBinding)
		if !ok {
			return fmt.Errorf("expected RoleBinding, got %T", obj)
		}
		applyAIGatewayMetadata(binding, aigw)
		binding.Subjects = subjects
		binding.RoleRef = rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     name,
		}
		return nil
	})
}

func (r *AIGatewayReconciler) rbacSubjects(aigw *maasv1alpha1.AIGateway) ([]rbacv1.Subject, error) {
	if aigw.Spec.RBAC == nil || len(aigw.Spec.RBAC.Admins) == 0 {
		return nil, nil
	}
	subjects := make([]rbacv1.Subject, 0, len(aigw.Spec.RBAC.Admins))
	for _, admin := range aigw.Spec.RBAC.Admins {
		subject := rbacv1.Subject{
			Kind: admin.Kind,
			Name: admin.Name,
		}
		switch admin.Kind {
		case rbacv1.UserKind, rbacv1.GroupKind:
			subject.APIGroup = rbacv1.GroupName
		case rbacv1.ServiceAccountKind:
			if admin.Namespace == "" {
				return nil, fmt.Errorf("spec.rbac.admins[%s].namespace is required for ServiceAccount subjects", admin.Name)
			}
			subject.Namespace = admin.Namespace
		default:
			return nil, fmt.Errorf("unsupported RBAC subject kind %q", admin.Kind)
		}
		subjects = append(subjects, subject)
	}
	return subjects, nil
}

func (r *AIGatewayReconciler) reconcileAIGatewayDelete(ctx context.Context, aigw *maasv1alpha1.AIGateway) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(aigw, aigatewayFinalizer) {
		return ctrl.Result{}, nil
	}
	if err := r.deleteAIGatewayChildren(ctx, aigw); err != nil {
		return ctrl.Result{}, err
	}
	base := aigw.DeepCopy()
	controllerutil.RemoveFinalizer(aigw, aigatewayFinalizer)
	if err := r.Patch(ctx, aigw, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *AIGatewayReconciler) deleteAIGatewayChildren(ctx context.Context, aigw *maasv1alpha1.AIGateway) error {
	gatewayRef := r.gatewayRefFor(aigw)
	if err := r.deleteOwned(ctx, aigw, &gatewayapiv1.Gateway{}, client.ObjectKey{Namespace: gatewayRef.Namespace, Name: gatewayRef.Name}); err != nil {
		return err
	}
	if err := r.deleteOwned(ctx, aigw, &maasv1alpha1.Tenant{}, client.ObjectKey{Namespace: aigw.Spec.TenantNamespace.Name, Name: maasv1alpha1.TenantInstanceName}); err != nil {
		return err
	}
	if err := r.deleteOwnedRoleBinding(ctx, aigw, aigw.Spec.TenantNamespace.Name, tenantAdminRoleName(aigw)); err != nil {
		return err
	}
	if err := r.deleteOwnedRoleBinding(ctx, aigw, aigw.Namespace, aigatewayAccessRoleName(aigw)); err != nil {
		return err
	}
	if err := r.deleteOwned(ctx, aigw, &rbacv1.Role{}, client.ObjectKey{Namespace: aigw.Spec.TenantNamespace.Name, Name: tenantAdminRoleName(aigw)}); err != nil {
		return err
	}
	if err := r.deleteOwned(ctx, aigw, &rbacv1.Role{}, client.ObjectKey{Namespace: aigw.Namespace, Name: aigatewayAccessRoleName(aigw)}); err != nil {
		return err
	}
	if !boolDefault(aigw.Spec.TenantNamespace.CleanupOnDelete, false) {
		return nil
	}
	var ns corev1.Namespace
	key := client.ObjectKey{Name: aigw.Spec.TenantNamespace.Name}
	if err := r.get(ctx, key, &ns); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !ownedByAIGateway(&ns, aigw) || ns.Annotations[aigatewayCreatedAnnotation] != "true" {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, &ns))
}

func (r *AIGatewayReconciler) deleteOwnedRoleBinding(ctx context.Context, aigw *maasv1alpha1.AIGateway, namespace, name string) error {
	return r.deleteOwned(ctx, aigw, &rbacv1.RoleBinding{}, client.ObjectKey{Namespace: namespace, Name: name})
}

func (r *AIGatewayReconciler) deleteOwned(ctx context.Context, aigw *maasv1alpha1.AIGateway, obj client.Object, key client.ObjectKey) error {
	if key.Name == "" {
		return nil
	}
	if err := r.get(ctx, key, obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !ownedByAIGateway(obj, aigw) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, obj))
}

func (r *AIGatewayReconciler) get(ctx context.Context, key client.ObjectKey, obj client.Object) error {
	if r.APIReader != nil {
		return r.APIReader.Get(ctx, key, obj)
	}
	return r.Get(ctx, key, obj)
}

func (r *AIGatewayReconciler) upsert(ctx context.Context, obj client.Object, aigw *maasv1alpha1.AIGateway, mutate func(client.Object) error) error {
	key := client.ObjectKeyFromObject(obj)
	current, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("expected client.Object copy, got %T", obj.DeepCopyObject())
	}
	err := r.get(ctx, key, current)
	if apierrors.IsNotFound(err) {
		if err := mutate(obj); err != nil {
			return err
		}
		if err := r.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create %s %s/%s: %w", objectKind(obj), key.Namespace, key.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get %s %s/%s: %w", objectKind(obj), key.Namespace, key.Name, err)
	}
	if hasAIGatewayOwnerAnnotations(current) && !ownedByAIGateway(current, aigw) {
		return fmt.Errorf("%s %s/%s is managed by another AIGateway", objectKind(obj), key.Namespace, key.Name)
	}
	base, ok := current.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("expected client.Object copy, got %T", current.DeepCopyObject())
	}
	if err := mutate(current); err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(base, current) {
		return nil
	}
	if err := r.Patch(ctx, current, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch %s %s/%s: %w", objectKind(obj), key.Namespace, key.Name, err)
	}
	return nil
}

func setAIGatewayPhase(aigw *maasv1alpha1.AIGateway, phase, reason, message string) {
	aigw.Status.Phase = phase
	status := metav1.ConditionFalse
	if phase == "Active" {
		status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&aigw.Status.Conditions, metav1.Condition{
		Type:               maasv1alpha1.AIGatewayConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: aigw.Generation,
		LastTransitionTime: metav1.Now(),
	})
}

func (r *AIGatewayReconciler) updateAIGatewayStatus(ctx context.Context, aigw *maasv1alpha1.AIGateway, statusSnapshot *maasv1alpha1.AIGatewayStatus) error {
	if equality.Semantic.DeepEqual(*statusSnapshot, aigw.Status) {
		return nil
	}
	return r.Status().Update(ctx, aigw)
}

func applyAIGatewayMetadata(obj client.Object, aigw *maasv1alpha1.AIGateway) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels["app.kubernetes.io/managed-by"] = "maas-controller"
	labels["app.kubernetes.io/part-of"] = tenantreconcile.ComponentName
	labels[aigatewayManagedLabel] = "true"
	labels[tenantreconcile.LabelTenantName] = aigw.Name
	labels[tenantreconcile.LabelTenantNamespace] = aigw.Spec.TenantNamespace.Name
	obj.SetLabels(labels)

	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[aigatewayNameAnnotation] = aigw.Name
	annotations[aigatewayNamespaceAnnotation] = aigw.Namespace
	obj.SetAnnotations(annotations)
}

func ownedByAIGateway(obj client.Object, aigw *maasv1alpha1.AIGateway) bool {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		return false
	}
	if aigw == nil {
		return annotations[aigatewayNameAnnotation] != "" && annotations[aigatewayNamespaceAnnotation] != ""
	}
	return annotations[aigatewayNameAnnotation] == aigw.Name &&
		annotations[aigatewayNamespaceAnnotation] == aigw.Namespace
}

func hasAIGatewayOwnerAnnotations(obj client.Object) bool {
	annotations := obj.GetAnnotations()
	return annotations != nil &&
		(annotations[aigatewayNameAnnotation] != "" || annotations[aigatewayNamespaceAnnotation] != "")
}

func setMapValue(m *map[string]string, key, value string) {
	if key == "" {
		return
	}
	if *m == nil {
		*m = map[string]string{}
	}
	(*m)[key] = value
}

func boolDefault(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

func tenantAdminRoleName(aigw *maasv1alpha1.AIGateway) string {
	return aigatewayChildName(aigw.Name, aigatewayTenantAdminRoleSuffix)
}

func aigatewayAccessRoleName(aigw *maasv1alpha1.AIGateway) string {
	return aigatewayChildName(aigw.Name, aigatewayAccessRoleSuffix)
}

func aigatewayChildName(aigwName, suffix string) string {
	const prefix = "aigateway-"
	name := prefix + aigwName + "-" + suffix
	if len(name) <= 63 {
		return name
	}
	sum := sha256.Sum256([]byte(aigwName))
	hash := hex.EncodeToString(sum[:])[:8]
	budget := 63 - len(prefix) - len(suffix) - len(hash) - 2
	if budget < 1 {
		return prefix + hash + "-" + suffix
	}
	trimmed := strings.Trim(aigwName[:budget], "-.")
	if trimmed == "" {
		trimmed = hash
	}
	return prefix + trimmed + "-" + suffix + "-" + hash
}

func objectKind(obj client.Object) string {
	if gvk := obj.GetObjectKind().GroupVersionKind(); gvk.Kind != "" {
		return gvk.Kind
	}
	t := fmt.Sprintf("%T", obj)
	if i := strings.LastIndex(t, "."); i >= 0 {
		return strings.TrimPrefix(t[i+1:], "*")
	}
	return strings.TrimPrefix(t, "*")
}
