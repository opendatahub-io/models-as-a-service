/*
Copyright 2026.

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
	// DefaultAITenantNamespace is the default infrastructure namespace for AITenant CRs.
	DefaultAITenantNamespace = "redhat-ai-gateway-infra"

	aitenantFinalizer = "maas.opendatahub.io/aitenant-cleanup"

	aitenantManagedLabel = "maas.opendatahub.io/managed-by-aitenant"
	aiGatewayTenantLabel = "ai-gateway.opendatahub.io/tenant"

	aitenantNameAnnotation      = "maas.opendatahub.io/aitenant-name"
	aitenantNamespaceAnnotation = "maas.opendatahub.io/aitenant-namespace"
	aitenantCreatedAnnotation   = "maas.opendatahub.io/created-by-aitenant"

	aitenantTenantAdminRoleSuffix = "tenant-admin"
	aitenantAccessRoleSuffix      = "object-admin"
)

// AITenantReconciler reconciles AITenant tenant bootstrap resources.
type AITenantReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader is used for reads that must bypass the Tenant namespace cache scope.
	APIReader client.Reader

	// AppNamespace is the protected ODH application namespace. AITenant objects
	// and tenant namespaces must not live there.
	AppNamespace string
	// TenantNamespace is the default MaaS tenant namespace. AITenant objects
	// must stay in a separate infra namespace, but they may target this namespace.
	TenantNamespace string
	// AITenantNamespace is the infrastructure namespace where AITenant CRs are accepted.
	AITenantNamespace string
	// GatewayNamespace is where tenant Gateway resources are expected to exist.
	GatewayNamespace string
}

// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=aitenants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=aitenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=aitenants/finalizers,verbs=update
// +kubebuilder:rbac:groups=maas.opendatahub.io,resources=tenants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives AITenant bootstrap lifecycle.
func (r *AITenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var aitenant maasv1alpha1.AITenant
	if err := r.Get(ctx, req.NamespacedName, &aitenant); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !aitenant.DeletionTimestamp.IsZero() {
		return r.reconcileAITenantDelete(ctx, &aitenant)
	}

	if !controllerutil.ContainsFinalizer(&aitenant, aitenantFinalizer) {
		base := aitenant.DeepCopy()
		controllerutil.AddFinalizer(&aitenant, aitenantFinalizer)
		if err := r.Patch(ctx, &aitenant, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	statusSnapshot := aitenant.Status.DeepCopy()

	if err := r.validateAITenantPlacement(&aitenant); err != nil {
		setAITenantPhase(&aitenant, "Failed", "InvalidPlacement", err.Error())
		return ctrl.Result{}, r.updateAITenantStatus(ctx, &aitenant, statusSnapshot)
	}

	aitenant.Status.TenantNamespace = aitenant.Spec.TenantNamespace.Name

	gatewayRef, err := r.validateTenantGateway(ctx, &aitenant)
	aitenant.Status.GatewayRef = gatewayRef
	if err != nil {
		setAITenantPhase(&aitenant, "Failed", "GatewayCheckFailed", err.Error())
		if err2 := r.updateAITenantStatus(ctx, &aitenant, statusSnapshot); err2 != nil {
			return ctrl.Result{}, err2
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if err := r.ensureTenantNamespace(ctx, &aitenant); err != nil {
		if errors.Is(err, errTenantNamespaceMissing) {
			setAITenantPhase(&aitenant, "Pending", "TenantNamespaceMissing", err.Error())
			if err2 := r.updateAITenantStatus(ctx, &aitenant, statusSnapshot); err2 != nil {
				return ctrl.Result{}, err2
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		setAITenantPhase(&aitenant, "Failed", "TenantNamespaceFailed", err.Error())
		if err2 := r.updateAITenantStatus(ctx, &aitenant, statusSnapshot); err2 != nil {
			return ctrl.Result{}, err2
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if err := r.ensureTenantConfig(ctx, &aitenant, gatewayRef); err != nil {
		setAITenantPhase(&aitenant, "Failed", "TenantConfigReconcileFailed", err.Error())
		if err2 := r.updateAITenantStatus(ctx, &aitenant, statusSnapshot); err2 != nil {
			return ctrl.Result{}, err2
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if err := r.ensureTenantAdminRBAC(ctx, &aitenant); err != nil {
		setAITenantPhase(&aitenant, "Failed", "RBACReconcileFailed", err.Error())
		if err2 := r.updateAITenantStatus(ctx, &aitenant, statusSnapshot); err2 != nil {
			return ctrl.Result{}, err2
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	setAITenantPhase(&aitenant, "Active", "Reconciled", "AITenant bootstrap resources are reconciled")
	if err := r.updateAITenantStatus(ctx, &aitenant, statusSnapshot); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the AITenant controller.
func (r *AITenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&maasv1alpha1.AITenant{}, builder.WithPredicates(
			predicate.Or(predicate.GenerationChangedPredicate{}, predicate.Funcs{UpdateFunc: deletionTimestampSet}),
		)).
		Complete(r)
}

func (r *AITenantReconciler) validateAITenantPlacement(aitenant *maasv1alpha1.AITenant) error {
	if aitenant.Namespace == "" {
		return fmt.Errorf("AITenant %q must be namespaced", aitenant.Name)
	}
	aitenantNamespace := r.aitenantNamespace()
	if r.AppNamespace != "" && aitenant.Namespace == r.AppNamespace {
		return fmt.Errorf("AITenant %s/%s must not be created in the protected application namespace %q", aitenant.Namespace, aitenant.Name, r.AppNamespace)
	}
	if r.TenantNamespace != "" && aitenant.Namespace == r.TenantNamespace {
		return fmt.Errorf("AITenant %s/%s must be created in a separate infra namespace, not the tenant namespace %q", aitenant.Namespace, aitenant.Name, r.TenantNamespace)
	}
	if aitenant.Namespace != aitenantNamespace {
		return fmt.Errorf("AITenant %s/%s must be created in the configured AITenant infrastructure namespace %q", aitenant.Namespace, aitenant.Name, aitenantNamespace)
	}
	if aitenant.Spec.TenantNamespace.Name == "" {
		return errors.New("spec.tenantNamespace.name is required")
	}
	if aitenant.Spec.TenantNamespace.Name == aitenant.Namespace {
		return fmt.Errorf("spec.tenantNamespace.name must be different from the AITenant infra namespace %q", aitenant.Namespace)
	}
	if r.AppNamespace != "" && aitenant.Spec.TenantNamespace.Name == r.AppNamespace {
		return fmt.Errorf("spec.tenantNamespace.name must not be the protected application namespace %q", r.AppNamespace)
	}
	return nil
}

func (r *AITenantReconciler) aitenantNamespace() string {
	if r.AITenantNamespace == "" {
		return DefaultAITenantNamespace
	}
	return r.AITenantNamespace
}

var errTenantNamespaceMissing = errors.New("tenant namespace missing")

func (r *AITenantReconciler) ensureTenantNamespace(ctx context.Context, aitenant *maasv1alpha1.AITenant) error {
	name := aitenant.Spec.TenantNamespace.Name
	var ns corev1.Namespace
	err := r.get(ctx, client.ObjectKey{Name: name}, &ns)
	if isNotFoundError(err) {
		if !boolDefault(aitenant.Spec.TenantNamespace.Create, true) {
			return fmt.Errorf("%w: namespace %q does not exist and spec.tenantNamespace.create=false", errTenantNamespaceMissing, name)
		}
		toCreate := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
		}
		applyAITenantMetadata(toCreate, aitenant)
		setMapValue(&toCreate.Labels, "opendatahub.io/generated-namespace", "true")
		setMapValue(&toCreate.Annotations, aitenantCreatedAnnotation, "true")
		if createErr := r.Create(ctx, toCreate); createErr != nil {
			if !isAlreadyExistsError(createErr) {
				return fmt.Errorf("create tenant namespace %q: %w", name, createErr)
			}
			if err := r.get(ctx, client.ObjectKey{Name: name}, &ns); err != nil {
				return fmt.Errorf("get tenant namespace %q after create conflict: %w", name, err)
			}
			err = nil
		} else {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("get tenant namespace %q: %w", name, err)
	}
	if ns.Status.Phase == corev1.NamespaceTerminating {
		return fmt.Errorf("tenant namespace %q is terminating", name)
	}
	if hasAITenantOwnerAnnotations(&ns) && !ownedByAITenant(&ns, aitenant) {
		return fmt.Errorf("tenant namespace %q is managed by another AITenant", name)
	}
	base := ns.DeepCopy()
	applyAITenantMetadata(&ns, aitenant)
	if equality.Semantic.DeepEqual(base, &ns) {
		return nil
	}
	if err := r.Patch(ctx, &ns, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch tenant namespace %q: %w", name, err)
	}
	return nil
}

func (r *AITenantReconciler) validateTenantGateway(ctx context.Context, aitenant *maasv1alpha1.AITenant) (maasv1alpha1.TenantGatewayRef, error) {
	ref := r.gatewayRefFor(aitenant)
	if ref.Namespace == "" {
		return ref, errors.New("gateway namespace is required; set --gateway-namespace")
	}
	if ref.Name == "" {
		return ref, errors.New("spec.gateway.name is required when AITenant name is empty")
	}

	var gateway gatewayapiv1.Gateway
	key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
	if err := r.get(ctx, key, &gateway); err != nil {
		if isNotFoundError(err) {
			return ref, fmt.Errorf("gateway %s/%s not found: the Gateway must be created by a network or cluster administrator before AITenant can be provisioned", key.Namespace, key.Name)
		}
		return ref, fmt.Errorf("get Gateway %s/%s: %w", key.Namespace, key.Name, err)
	}
	return ref, nil
}

func (r *AITenantReconciler) gatewayRefFor(aitenant *maasv1alpha1.AITenant) maasv1alpha1.TenantGatewayRef {
	ref := maasv1alpha1.TenantGatewayRef{
		Namespace: r.GatewayNamespace,
		Name:      aitenant.Name,
	}
	if aitenant.Spec.Gateway != nil {
		if aitenant.Spec.Gateway.Name != "" {
			ref.Name = aitenant.Spec.Gateway.Name
		}
	}
	return ref
}

func (r *AITenantReconciler) ensureTenantConfig(ctx context.Context, aitenant *maasv1alpha1.AITenant, gatewayRef maasv1alpha1.TenantGatewayRef) error {
	tenant := &maasv1alpha1.Tenant{
		TypeMeta: metav1.TypeMeta{
			APIVersion: maasv1alpha1.GroupVersion.String(),
			Kind:       maasv1alpha1.TenantKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.TenantInstanceName,
			Namespace: aitenant.Spec.TenantNamespace.Name,
		},
	}
	return r.upsert(ctx, tenant, aitenant, func(obj client.Object) error {
		t, ok := obj.(*maasv1alpha1.Tenant)
		if !ok {
			return fmt.Errorf("expected Tenant, got %T", obj)
		}
		applyAITenantMetadata(t, aitenant)
		// TODO: Move these mirrored platform values out of Tenant spec in a
		// follow-up Jira once the MaaS config/status API is settled. The current
		// post-render path still reads Tenant.spec.gatewayRef and externalOIDC.
		t.Spec.GatewayRef = gatewayRef
		t.Spec.ExternalOIDC = aitenant.Spec.OIDC
		return nil
	})
}

func (r *AITenantReconciler) ensureTenantAdminRBAC(ctx context.Context, aitenant *maasv1alpha1.AITenant) error {
	subjects, err := r.rbacSubjects(aitenant)
	if err != nil {
		return err
	}
	if err := r.ensureTenantNamespaceRole(ctx, aitenant); err != nil {
		return err
	}
	if err := r.ensureAITenantObjectRole(ctx, aitenant); err != nil {
		return err
	}

	if len(subjects) == 0 {
		if err := r.deleteOwnedRoleBinding(ctx, aitenant, aitenant.Spec.TenantNamespace.Name, tenantAdminRoleName(aitenant)); err != nil {
			return err
		}
		return r.deleteOwnedRoleBinding(ctx, aitenant, aitenant.Namespace, aitenantAccessRoleName(aitenant))
	}
	if err := r.ensureRoleBinding(ctx, aitenant, aitenant.Spec.TenantNamespace.Name, tenantAdminRoleName(aitenant), subjects); err != nil {
		return err
	}
	return r.ensureRoleBinding(ctx, aitenant, aitenant.Namespace, aitenantAccessRoleName(aitenant), subjects)
}

func (r *AITenantReconciler) ensureTenantNamespaceRole(ctx context.Context, aitenant *maasv1alpha1.AITenant) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantAdminRoleName(aitenant),
			Namespace: aitenant.Spec.TenantNamespace.Name,
		},
	}
	return r.upsert(ctx, role, aitenant, func(obj client.Object) error {
		role, ok := obj.(*rbacv1.Role)
		if !ok {
			return fmt.Errorf("expected Role, got %T", obj)
		}
		applyAITenantMetadata(role, aitenant)
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

func (r *AITenantReconciler) ensureAITenantObjectRole(ctx context.Context, aitenant *maasv1alpha1.AITenant) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      aitenantAccessRoleName(aitenant),
			Namespace: aitenant.Namespace,
		},
	}
	return r.upsert(ctx, role, aitenant, func(obj client.Object) error {
		role, ok := obj.(*rbacv1.Role)
		if !ok {
			return fmt.Errorf("expected Role, got %T", obj)
		}
		applyAITenantMetadata(role, aitenant)
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups:     []string{maasv1alpha1.GroupVersion.Group},
				Resources:     []string{"aitenants"},
				ResourceNames: []string{aitenant.Name},
				Verbs:         []string{"get"},
			},
		}
		return nil
	})
}

func (r *AITenantReconciler) ensureRoleBinding(ctx context.Context, aitenant *maasv1alpha1.AITenant, namespace, name string, subjects []rbacv1.Subject) error {
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	return r.upsert(ctx, binding, aitenant, func(obj client.Object) error {
		binding, ok := obj.(*rbacv1.RoleBinding)
		if !ok {
			return fmt.Errorf("expected RoleBinding, got %T", obj)
		}
		applyAITenantMetadata(binding, aitenant)
		binding.Subjects = subjects
		binding.RoleRef = rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     name,
		}
		return nil
	})
}

func (r *AITenantReconciler) rbacSubjects(aitenant *maasv1alpha1.AITenant) ([]rbacv1.Subject, error) {
	if aitenant.Spec.RBAC == nil || len(aitenant.Spec.RBAC.Admins) == 0 {
		return nil, nil
	}
	subjects := make([]rbacv1.Subject, 0, len(aitenant.Spec.RBAC.Admins))
	for _, admin := range aitenant.Spec.RBAC.Admins {
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

func (r *AITenantReconciler) reconcileAITenantDelete(ctx context.Context, aitenant *maasv1alpha1.AITenant) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(aitenant, aitenantFinalizer) {
		return ctrl.Result{}, nil
	}
	if err := r.deleteAITenantChildren(ctx, aitenant); err != nil {
		return ctrl.Result{}, err
	}
	base := aitenant.DeepCopy()
	controllerutil.RemoveFinalizer(aitenant, aitenantFinalizer)
	if err := r.Patch(ctx, aitenant, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *AITenantReconciler) deleteAITenantChildren(ctx context.Context, aitenant *maasv1alpha1.AITenant) error {
	if err := r.deleteOwned(ctx, aitenant, &maasv1alpha1.Tenant{}, client.ObjectKey{Namespace: aitenant.Spec.TenantNamespace.Name, Name: maasv1alpha1.TenantInstanceName}); err != nil {
		return err
	}
	if err := r.deleteOwnedRoleBinding(ctx, aitenant, aitenant.Spec.TenantNamespace.Name, tenantAdminRoleName(aitenant)); err != nil {
		return err
	}
	if err := r.deleteOwnedRoleBinding(ctx, aitenant, aitenant.Namespace, aitenantAccessRoleName(aitenant)); err != nil {
		return err
	}
	if err := r.deleteOwned(ctx, aitenant, &rbacv1.Role{}, client.ObjectKey{Namespace: aitenant.Spec.TenantNamespace.Name, Name: tenantAdminRoleName(aitenant)}); err != nil {
		return err
	}
	if err := r.deleteOwned(ctx, aitenant, &rbacv1.Role{}, client.ObjectKey{Namespace: aitenant.Namespace, Name: aitenantAccessRoleName(aitenant)}); err != nil {
		return err
	}
	return r.cleanupTenantNamespaceMetadata(ctx, aitenant)
}

func (r *AITenantReconciler) cleanupTenantNamespaceMetadata(ctx context.Context, aitenant *maasv1alpha1.AITenant) error {
	if aitenant.Spec.TenantNamespace.Name == "" {
		return nil
	}
	var ns corev1.Namespace
	key := client.ObjectKey{Name: aitenant.Spec.TenantNamespace.Name}
	if err := r.get(ctx, key, &ns); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !ownedByAITenant(&ns, aitenant) {
		return nil
	}
	base := ns.DeepCopy()
	removeAITenantMetadata(&ns, aitenant)
	removeMapValueIfEqual(&ns.Labels, "opendatahub.io/generated-namespace", "true")
	if equality.Semantic.DeepEqual(base, &ns) {
		return nil
	}
	if err := r.Patch(ctx, &ns, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("cleanup tenant namespace %q metadata: %w", key.Name, err)
	}
	return nil
}

func (r *AITenantReconciler) deleteOwnedRoleBinding(ctx context.Context, aitenant *maasv1alpha1.AITenant, namespace, name string) error {
	return r.deleteOwned(ctx, aitenant, &rbacv1.RoleBinding{}, client.ObjectKey{Namespace: namespace, Name: name})
}

func (r *AITenantReconciler) deleteOwned(ctx context.Context, aitenant *maasv1alpha1.AITenant, obj client.Object, key client.ObjectKey) error {
	if key.Name == "" {
		return nil
	}
	if err := r.get(ctx, key, obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !ownedByAITenant(obj, aitenant) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, obj))
}

func (r *AITenantReconciler) get(ctx context.Context, key client.ObjectKey, obj client.Object) error {
	if r.APIReader != nil {
		return r.APIReader.Get(ctx, key, obj)
	}
	return r.Get(ctx, key, obj)
}

func (r *AITenantReconciler) upsert(ctx context.Context, obj client.Object, aitenant *maasv1alpha1.AITenant, mutate func(client.Object) error) error {
	return r.upsertWithCreate(ctx, obj, aitenant, mutate, nil)
}

func (r *AITenantReconciler) upsertWithCreate(ctx context.Context, obj client.Object, aitenant *maasv1alpha1.AITenant, mutate, mutateCreate func(client.Object) error) error {
	key := client.ObjectKeyFromObject(obj)
	current, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("expected client.Object copy, got %T", obj.DeepCopyObject())
	}
	err := r.get(ctx, key, current)
	if err != nil {
		if !isNotFoundError(err) {
			return fmt.Errorf("get %s %s/%s: %w", objectKind(obj), key.Namespace, key.Name, err)
		}
		if err := mutate(obj); err != nil {
			return err
		}
		if mutateCreate != nil {
			if err := mutateCreate(obj); err != nil {
				return err
			}
		}
		if createErr := r.Create(ctx, obj); createErr != nil {
			if !isAlreadyExistsError(createErr) {
				return fmt.Errorf("create %s %s/%s: %w", objectKind(obj), key.Namespace, key.Name, createErr)
			}
			if err := r.get(ctx, key, current); err != nil {
				return fmt.Errorf("get %s %s/%s after create conflict: %w", objectKind(obj), key.Namespace, key.Name, err)
			}
		} else {
			return nil
		}
	}
	if hasAITenantOwnerAnnotations(current) && !ownedByAITenant(current, aitenant) {
		return fmt.Errorf("%s %s/%s is managed by another AITenant", objectKind(obj), key.Namespace, key.Name)
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

func setAITenantPhase(aitenant *maasv1alpha1.AITenant, phase, reason, message string) {
	aitenant.Status.Phase = phase
	status := metav1.ConditionFalse
	if phase == "Active" {
		status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&aitenant.Status.Conditions, metav1.Condition{
		Type:               maasv1alpha1.AITenantConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: aitenant.Generation,
		LastTransitionTime: metav1.Now(),
	})
}

func (r *AITenantReconciler) updateAITenantStatus(ctx context.Context, aitenant *maasv1alpha1.AITenant, statusSnapshot *maasv1alpha1.AITenantStatus) error {
	if equality.Semantic.DeepEqual(*statusSnapshot, aitenant.Status) {
		return nil
	}
	return r.Status().Update(ctx, aitenant)
}

func applyAITenantMetadata(obj client.Object, aitenant *maasv1alpha1.AITenant) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels["app.kubernetes.io/managed-by"] = "maas-controller"
	labels["app.kubernetes.io/part-of"] = tenantreconcile.ComponentName
	labels[aitenantManagedLabel] = "true"
	labels[aiGatewayTenantLabel] = aitenant.Name
	labels[tenantreconcile.LabelTenantName] = aitenant.Name
	labels[tenantreconcile.LabelTenantNamespace] = aitenant.Spec.TenantNamespace.Name
	obj.SetLabels(labels)

	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[aitenantNameAnnotation] = aitenant.Name
	annotations[aitenantNamespaceAnnotation] = aitenant.Namespace
	obj.SetAnnotations(annotations)
}

func removeAITenantMetadata(obj client.Object, aitenant *maasv1alpha1.AITenant) {
	labels := obj.GetLabels()
	removeMapValueIfEqual(&labels, "app.kubernetes.io/managed-by", "maas-controller")
	removeMapValueIfEqual(&labels, "app.kubernetes.io/part-of", tenantreconcile.ComponentName)
	removeMapValueIfEqual(&labels, aitenantManagedLabel, "true")
	removeMapValueIfEqual(&labels, aiGatewayTenantLabel, aitenant.Name)
	removeMapValueIfEqual(&labels, tenantreconcile.LabelTenantName, aitenant.Name)
	removeMapValueIfEqual(&labels, tenantreconcile.LabelTenantNamespace, aitenant.Spec.TenantNamespace.Name)
	obj.SetLabels(labels)

	annotations := obj.GetAnnotations()
	removeMapValueIfEqual(&annotations, aitenantNameAnnotation, aitenant.Name)
	removeMapValueIfEqual(&annotations, aitenantNamespaceAnnotation, aitenant.Namespace)
	removeMapValueIfEqual(&annotations, aitenantCreatedAnnotation, "true")
	obj.SetAnnotations(annotations)
}

func ownedByAITenant(obj client.Object, aitenant *maasv1alpha1.AITenant) bool {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		return false
	}
	if aitenant == nil {
		return annotations[aitenantNameAnnotation] != "" && annotations[aitenantNamespaceAnnotation] != ""
	}
	return annotations[aitenantNameAnnotation] == aitenant.Name &&
		annotations[aitenantNamespaceAnnotation] == aitenant.Namespace
}

func isNotFoundError(err error) bool {
	if apierrors.IsNotFound(err) {
		return true
	}
	if apierrors.ReasonForError(err) == metav1.StatusReasonNotFound {
		return true
	}
	return hasAPIStatusReason(err, metav1.StatusReasonNotFound)
}

func isAlreadyExistsError(err error) bool {
	if apierrors.IsAlreadyExists(err) {
		return true
	}
	if apierrors.ReasonForError(err) == metav1.StatusReasonAlreadyExists {
		return true
	}
	return hasAPIStatusReason(err, metav1.StatusReasonAlreadyExists)
}

func hasAPIStatusReason(err error, reason metav1.StatusReason) bool {
	for err != nil {
		status, ok := err.(apierrors.APIStatus)
		if ok && status.Status().Reason == reason {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}

func hasAITenantOwnerAnnotations(obj client.Object) bool {
	annotations := obj.GetAnnotations()
	return annotations != nil &&
		(annotations[aitenantNameAnnotation] != "" || annotations[aitenantNamespaceAnnotation] != "")
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

func removeMapValueIfEqual(m *map[string]string, key, value string) {
	if *m == nil {
		return
	}
	if (*m)[key] == value {
		delete(*m, key)
	}
	if len(*m) == 0 {
		*m = nil
	}
}

func boolDefault(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

func tenantAdminRoleName(aitenant *maasv1alpha1.AITenant) string {
	return aitenantChildName(aitenant.Name, aitenantTenantAdminRoleSuffix)
}

func aitenantAccessRoleName(aitenant *maasv1alpha1.AITenant) string {
	return aitenantChildName(aitenant.Name, aitenantAccessRoleSuffix)
}

func aitenantChildName(aitenantName, suffix string) string {
	const prefix = "aitenant-"
	name := prefix + aitenantName + "-" + suffix
	if len(name) <= 63 {
		return name
	}
	sum := sha256.Sum256([]byte(aitenantName))
	hash := hex.EncodeToString(sum[:])[:8]
	budget := 63 - len(prefix) - len(suffix) - len(hash) - 2
	if budget < 1 {
		return prefix + hash + "-" + suffix
	}
	trimmed := strings.Trim(aitenantName[:budget], "-.")
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
