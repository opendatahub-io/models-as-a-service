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

package main

import (
	"context"
	stderrors "errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/controller/maas"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/reconciler/externalmodel"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/webhook"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

const defaultAITenantBootstrappedAnnotation = "maas.opendatahub.io/default-aitenant-bootstrapped"

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(extv1.AddToScheme(scheme))
	utilruntime.Must(kservev1alpha1.AddToScheme(scheme))
	utilruntime.Must(gatewayapiv1.Install(scheme))
	utilruntime.Must(maasv1alpha1.AddToScheme(scheme))
}

//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;create

// ensureManagedNamespaceWithClient checks whether a controller-managed namespace exists
// and creates it if missing. It checks for existence first so that the controller can
// start even when the service account lacks namespace-create permission (common in
// operator-managed deployments where the operator pre-creates the namespace).
// Permanent errors such as Forbidden are not retried.
//
// Handles the edge case where the namespace is in Terminating phase during RHOAI
// reinstall/upgrade - waits for deletion to complete before attempting creation.
func ensureManagedNamespaceWithClient(ctx context.Context, namespace, purpose string, clientset kubernetes.Interface) error {
	ns, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err == nil {
		if ns.Status.Phase == corev1.NamespaceTerminating {
			setupLog.Info("managed namespace is terminating, waiting for deletion to complete",
				"namespace", namespace, "purpose", purpose)

			pollErr := wait.PollUntilContextTimeout(ctx, 2*time.Second, 90*time.Second, true,
				func(ctx context.Context) (bool, error) {
					checkNs, pollErr := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
					if errors.IsNotFound(pollErr) {
						setupLog.Info("terminating namespace has been deleted", "namespace", namespace, "purpose", purpose)
						return true, nil
					}
					if errors.IsForbidden(pollErr) {
						setupLog.Info("insufficient permissions to poll namespace deletion status, "+
							"assuming namespace is managed externally",
							"namespace", namespace, "purpose", purpose, "error", pollErr)
						return true, nil
					}
					if pollErr != nil {
						return false, fmt.Errorf("error checking namespace status during deletion wait: %w", pollErr)
					}
					if checkNs.Status.Phase == corev1.NamespaceActive || checkNs.Status.Phase == "" {
						setupLog.Info("managed namespace became active during deletion wait "+
							"(recreated by operator or external process)",
							"namespace", namespace, "purpose", purpose)
						return true, nil
					}
					setupLog.V(1).Info("namespace still terminating, will retry",
						"namespace", namespace, "purpose", purpose, "phase", checkNs.Status.Phase)
					return false, nil
				})

			if pollErr != nil {
				return fmt.Errorf("failed waiting for terminating namespace %q to be deleted: %w",
					namespace, pollErr)
			}

			finalNs, finalErr := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
			doneErr, fallThrough := resolveNamespaceAfterTerminationWait(namespace, finalNs, finalErr)
			if fallThrough {
				err = finalErr
			} else {
				if doneErr != nil {
					return doneErr
				}
				return nil
			}
		} else {
			setupLog.Info("managed namespace already exists",
				"namespace", namespace, "purpose", purpose, "phase", ns.Status.Phase)
			return nil
		}
	}

	if errors.IsForbidden(err) {
		setupLog.Info("insufficient permissions to check namespace existence, assuming it exists — "+
			"verify that the ClusterRoleBinding references the correct namespace for the controller ServiceAccount",
			"namespace", namespace, "purpose", purpose, "error", err)
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("unable to check if namespace %q exists: %w", namespace, err)
	}

	setupLog.Info("managed namespace not found, attempting to create it", "namespace", namespace, "purpose", purpose)
	return wait.ExponentialBackoffWithContext(ctx, wait.Backoff{
		Steps:    5,
		Duration: 1 * time.Second,
		Factor:   2.0,
	}, func(ctx context.Context) (bool, error) {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
				Labels: map[string]string{
					"opendatahub.io/generated-namespace": "true",
					"app.kubernetes.io/managed-by":       "maas-controller",
					"app.kubernetes.io/part-of":          "maas-controller",
				},
			},
		}

		_, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
		if err == nil {
			setupLog.Info("managed namespace ready", "namespace", namespace, "purpose", purpose)
			return true, nil
		}
		if errors.IsAlreadyExists(err) {
			// Re-check phase: AlreadyExists only proves the name is occupied, but the namespace
			// could still be Terminating. Verify it's actually ready before returning success.
			existingNs, getErr := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
			if getErr != nil {
				setupLog.Info("namespace already exists but failed to verify phase, will retry",
					"namespace", namespace, "error", getErr)
				return false, nil
			}
			if existingNs.Status.Phase == corev1.NamespaceActive || existingNs.Status.Phase == "" {
				setupLog.Info("managed namespace ready", "namespace", namespace, "purpose", purpose)
				return true, nil
			}
			setupLog.Info("namespace already exists but is not ready, will retry",
				"namespace", namespace, "purpose", purpose, "phase", existingNs.Status.Phase)
			return false, nil
		}
		if errors.IsForbidden(err) {
			return false, fmt.Errorf("service account lacks permission to create namespace %q — "+
				"either pre-create the namespace or grant 'create' on namespaces to the controller service account: %w",
				namespace, err)
		}
		setupLog.Info("retrying namespace creation", "namespace", namespace, "purpose", purpose, "error", err)
		return false, nil // transient error, retry
	})
}

func ensureSubscriptionNamespaceWithClient(ctx context.Context, namespace string, clientset kubernetes.Interface) error {
	return ensureManagedNamespaceWithClient(ctx, namespace, "subscription", clientset)
}

func ensureAITenantNamespaceWithClient(ctx context.Context, namespace string, clientset kubernetes.Interface) error {
	return ensureManagedNamespaceWithClient(ctx, namespace, "aitenant", clientset)
}

// resolveNamespaceAfterTerminationWait interprets the namespace GET after a successful termination poll.
// If fallThroughToCreate is true, the caller must assign the original finalErr to the outer GET error and
// continue into namespace creation. If fallThroughToCreate is false and the returned error is nil, the
// managed namespace is already satisfied (Active or assumed external management).
func resolveNamespaceAfterTerminationWait(namespace string, finalNs *corev1.Namespace, finalErr error) (doneErr error, fallThroughToCreate bool) {
	if finalErr == nil && (finalNs.Status.Phase == corev1.NamespaceActive || finalNs.Status.Phase == "") {
		setupLog.Info("subscription namespace exists and is active "+
			"(recreated externally during deletion wait)",
			"namespace", namespace)
		return nil, false
	}
	if errors.IsForbidden(finalErr) {
		setupLog.Info("insufficient permissions to verify namespace state after deletion wait, "+
			"assuming it exists",
			"namespace", namespace, "error", finalErr)
		return nil, false
	}
	if errors.IsNotFound(finalErr) {
		return nil, true
	}
	if finalErr != nil {
		return fmt.Errorf("unable to verify namespace %q after termination wait: %w", namespace, finalErr), false
	}
	if finalNs.Status.Phase == corev1.NamespaceTerminating {
		return fmt.Errorf("namespace %q is still terminating after wait; retry after it is fully deleted",
			namespace), false
	}
	return fmt.Errorf("namespace %q exists in unexpected state after termination wait (phase=%q)",
		namespace, finalNs.Status.Phase), false
}

// checkSubscriptionNamespaceReady returns nil if the subscription namespace exists and controllers can rely on it.
// Terminating and missing namespaces are not ready. Forbidden on GET matches startup behavior (assume operator-managed).
//
// Namespace.Status.Phase is documented as Active or Terminating; an empty string is treated as ready because it is
// commonly seen before status is fully populated and matches Kubernetes' defaulting to an active namespace.
func checkSubscriptionNamespaceReady(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	ns, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return fmt.Errorf("subscription namespace %q does not exist", namespace)
	}
	if errors.IsForbidden(err) {
		setupLog.V(1).Info("readiness: insufficient permissions to check namespace, assuming ready", "namespace", namespace, "error", err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("subscription namespace %q ready check: %w", namespace, err)
	}
	if ns.Status.Phase == corev1.NamespaceTerminating {
		return fmt.Errorf("subscription namespace %q is terminating", namespace)
	}
	if ns.Status.Phase == corev1.NamespaceActive || ns.Status.Phase == "" {
		return nil
	}
	return fmt.Errorf("subscription namespace %q is not ready (phase=%q)", namespace, ns.Status.Phase)
}

// subscriptionNamespaceReadiness performs an uncached Namespace GET on each probe for an accurate signal.
// Load is bounded by the kubelet readiness probe interval (often ~10s); avoid short-lived caching here so
// Terminating / deleted namespaces are reflected promptly.
func subscriptionNamespaceReadiness(clientset kubernetes.Interface, namespace string) healthz.Checker {
	return func(req *http.Request) error {
		return checkSubscriptionNamespaceReady(req.Context(), clientset, namespace)
	}
}

// managedNamespaceMonitor periodically re-runs ensureManagedNamespaceWithClient so a namespace
// removed while the process is running can be recreated. When leader election is enabled, only the leader runs this.
type managedNamespaceMonitor struct {
	clientset          kubernetes.Interface
	namespace          string
	purpose            string
	interval           time.Duration
	needLeaderElection bool
}

func (m *managedNamespaceMonitor) NeedLeaderElection() bool {
	return m.needLeaderElection
}

func (m *managedNamespaceMonitor) Start(ctx context.Context) error {
	if m.interval <= 0 {
		return fmt.Errorf("managed namespace maintain interval must be positive, got %v", m.interval)
	}
	run := func() {
		innerCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if err := ensureManagedNamespaceWithClient(innerCtx, m.namespace, m.purpose, m.clientset); err != nil {
			// Keep running; the next tick will retry. Alerting on sustained failure is better done via
			// metrics (e.g. Prometheus counter) in a follow-up if product needs it.
			setupLog.Error(err, "managed namespace maintenance failed", "namespace", m.namespace, "purpose", m.purpose)
		}
	}
	run()
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			run()
		}
	}
}

// getClusterServiceAccountIssuer fetches the cluster's service account issuer from OpenShift/ROSA configuration.
// Returns empty string if not found or not running on OpenShift/ROSA.
// Uses client.Reader (not client.Client) so it can be called before the manager cache starts.
func getClusterServiceAccountIssuer(c client.Reader) (string, error) {
	// Try to fetch the OpenShift Authentication config resource
	// This works on OpenShift/ROSA but not on vanilla Kubernetes
	authConfig := &unstructured.Unstructured{}
	authConfig.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "config.openshift.io",
		Version: "v1",
		Kind:    "Authentication",
	})

	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, authConfig); err != nil {
		return "", err
	}

	// Extract spec.serviceAccountIssuer
	issuer, found, err := unstructured.NestedString(authConfig.Object, "spec", "serviceAccountIssuer")
	if err != nil {
		return "", err
	}
	if !found || issuer == "" {
		return "", nil
	}

	return issuer, nil
}

// ensureDefaultAITenantBootstrap creates the default AITenant once per
// Config/default anchor after the controller Deployment is live. It intentionally
// creates only the AITenant shell; the AITenant reconciler owns creation/adoption
// of the namespace-local Tenant/default-tenant bridge object. Once bootstrapped,
// the Config annotation prevents recreating a default AITenant that an admin
// deletes intentionally.
func ensureDefaultAITenantBootstrap(ctx context.Context, c client.Client, tenantNamespace, aitenantNamespace, controllerDeploymentNS, controllerDeploymentName, gatewayName, gatewayNamespace string) (bool, error) {
	depKey := types.NamespacedName{Namespace: controllerDeploymentNS, Name: controllerDeploymentName}
	var dep appsv1.Deployment
	if err := c.Get(ctx, depKey, &dep); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get maas-controller Deployment for bootstrap gate: %w", err)
	}
	if !dep.DeletionTimestamp.IsZero() {
		return false, nil
	}

	ctKey := client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}
	var ct maasv1alpha1.Config
	if err := c.Get(ctx, ctKey, &ct); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get Config for bootstrap gate: %w", err)
	}
	if !ct.DeletionTimestamp.IsZero() || ct.UID == "" {
		return false, nil
	}

	aitenantKey := client.ObjectKey{Name: tenantreconcile.DefaultAITenantName, Namespace: aitenantNamespace}
	var existing maasv1alpha1.AITenant
	if err := c.Get(ctx, aitenantKey, &existing); err != nil {
		if !errors.IsNotFound(err) {
			return false, fmt.Errorf("get default AITenant: %w", err)
		}
	} else {
		if err := markDefaultAITenantBootstrapped(ctx, c, &ct); err != nil {
			return false, err
		}
		return false, nil
	}

	if ct.Annotations[defaultAITenantBootstrappedAnnotation] == "true" {
		return false, nil
	}

	aitenant := &maasv1alpha1.AITenant{
		TypeMeta: metav1.TypeMeta{
			APIVersion: maasv1alpha1.GroupVersion.String(),
			Kind:       maasv1alpha1.AITenantKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantreconcile.DefaultAITenantName,
			Namespace: aitenantNamespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: maasv1alpha1.GroupVersion.String(),
					Kind:       maasv1alpha1.ConfigKind,
					Name:       ct.Name,
					UID:        ct.UID,
				},
			},
		},
		Spec: maasv1alpha1.AITenantSpec{
			Gateway: &maasv1alpha1.AITenantGatewayRef{Name: gatewayName},
		},
	}

	var tenant maasv1alpha1.Tenant
	tenantKey := client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: tenantNamespace}
	if err := c.Get(ctx, tenantKey, &tenant); err != nil {
		if !errors.IsNotFound(err) {
			return false, fmt.Errorf("get existing default Tenant for migration: %w", err)
		}
	} else {
		if tenant.Spec.ExternalOIDC != nil {
			oidc := *tenant.Spec.ExternalOIDC
			aitenant.Spec.OIDC = &oidc
		}
		if tenant.Spec.GatewayRef.Name != "" {
			// AITenant carries only the Gateway name; the namespace is controller configuration.
			// Preserve an existing custom name so a mismatched namespace fails visibly instead
			// of silently switching the default tenant back to the flag-default Gateway name.
			aitenant.Spec.Gateway.Name = tenant.Spec.GatewayRef.Name
		}
	}

	if err := c.Create(ctx, aitenant); err != nil {
		if errors.IsAlreadyExists(err) {
			return false, nil
		}
		return false, fmt.Errorf("create default AITenant: %w", err)
	}
	if err := markDefaultAITenantBootstrapped(ctx, c, &ct); err != nil {
		return true, err
	}
	return true, nil
}

func markDefaultAITenantBootstrapped(ctx context.Context, c client.Client, ct *maasv1alpha1.Config) error {
	if ct == nil || ct.Annotations[defaultAITenantBootstrappedAnnotation] == "true" {
		return nil
	}
	base := ct.DeepCopy()
	annotations := ct.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[defaultAITenantBootstrappedAnnotation] = "true"
	ct.SetAnnotations(annotations)
	if err := c.Patch(ctx, ct, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("mark default AITenant bootstrap on Config/default: %w", err)
	}
	return nil
}

// ensureClusterBootstrapRunnable bootstraps the default AITenant once
// Config/default is present (created by LifecycleReconciler). It does not set
// owner references; LifecycleReconciler links Config to the default AITenant,
// Deployment, and default Tenant. It does not create while the maas-controller
// Deployment is terminating, so bootstrap does not fight teardown.
func ensureClusterBootstrapRunnable(mgr ctrl.Manager, tenantNamespace, aitenantNamespace, controllerDeploymentNS, controllerDeploymentName, gatewayName, gatewayNamespace string) manager.RunnableFunc {
	return func(ctx context.Context) error {
		log := ctrl.Log.WithName("setup").WithName("ensureClusterBootstrap")
		c := mgr.GetClient()

		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		ensure := func() {
			created, err := ensureDefaultAITenantBootstrap(ctx, c, tenantNamespace, aitenantNamespace, controllerDeploymentNS, controllerDeploymentName, gatewayName, gatewayNamespace)
			if err != nil {
				log.Error(err, "failed to ensure default AITenant")
				return
			}
			if created {
				log.Info("ensured default AITenant exists",
					"name", tenantreconcile.DefaultAITenantName,
					"namespace", aitenantNamespace,
					"tenantNamespace", tenantNamespace,
					"gatewayNamespace", gatewayNamespace,
					"gatewayName", gatewayName)
			}
		}

		ensure()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				ensure()
			}
		}
	}
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var gatewayName string
	var gatewayNamespace string
	var controllerNamespace string
	var maasAPINamespace string
	var maasSubscriptionNamespace string
	var aitenantNamespace string
	var metadataCacheTTL int64
	var authzCacheTTL int64
	var subscriptionNamespaceMaintainInterval time.Duration
	var enableTenantNamespaceDiscovery bool
	var observabilityManifestsPath string
	var monitoringNamespace string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager.")
	flag.StringVar(&gatewayName, "gateway-name", "maas-default-gateway", "The name of the Gateway resource to use for model HTTPRoutes.")
	flag.StringVar(&gatewayNamespace, "gateway-namespace", "openshift-ingress", "The namespace of the Gateway resource.")
	flag.StringVar(&controllerNamespace, "controller-namespace", "opendatahub", "The namespace where the maas-controller Deployment runs.")
	flag.StringVar(&maasAPINamespace, "maas-api-namespace", tenantreconcile.DefaultMaaSAPINamespace, "The namespace where maas-api service is deployed.")
	flag.StringVar(&observabilityManifestsPath, "observability-manifests-path", "/deployment/components/observability/observability/dashboards", "Path to observability dashboard kustomize manifests.")
	flag.StringVar(&monitoringNamespace, "monitoring-namespace", "opendatahub", "The namespace where the monitoring stack is deployed.")
	flag.StringVar(&maasSubscriptionNamespace, "maas-subscription-namespace", "models-as-a-service", "The namespace to watch for MaaS CRs.")
	flag.StringVar(&aitenantNamespace, "aitenant-namespace", tenantreconcile.DefaultAITenantNamespace, "The infrastructure namespace where AITenant CRs are accepted.")
	flag.Int64Var(&metadataCacheTTL, "metadata-cache-ttl", 60, "TTL in seconds for Authorino metadata HTTP caching (apiKeyValidation, subscription-info).")
	flag.Int64Var(&authzCacheTTL, "authz-cache-ttl", 60, "TTL in seconds for Authorino OPA authorization caching (auth-valid, subscription-valid, require-group-membership).")
	flag.DurationVar(&subscriptionNamespaceMaintainInterval, "subscription-namespace-maintain-interval", 30*time.Second,
		"How often to re-check controller-managed namespaces while the manager is running (recreate if deleted). "+
			"Larger values reduce apiserver load; smaller values detect external deletions sooner.")
	flag.BoolVar(&enableTenantNamespaceDiscovery, "enable-tenant-namespace-discovery", false,
		"Discover AITenant-managed tenant namespaces labeled ai-gateway.opendatahub.io/tenant or maas.opendatahub.io/managed-by-aitenant=true and reconcile MaaS tenant CRs from them.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if errs := validation.IsDNS1123Label(monitoringNamespace); len(errs) > 0 {
		setupLog.Error(stderrors.New("invalid monitoring namespace"),
			"--monitoring-namespace must be a valid Kubernetes namespace name",
			"namespace", monitoringNamespace, "errors", errs)
		os.Exit(1)
	}

	if gatewayName == "" || gatewayNamespace == "" {
		setupLog.Error(stderrors.New("invalid gateway configuration"),
			"both --gateway-name and --gateway-namespace must be non-empty",
			"gatewayName", gatewayName, "gatewayNamespace", gatewayNamespace)
		os.Exit(1)
	}
	if strings.TrimSpace(controllerNamespace) == "" {
		setupLog.Error(stderrors.New("invalid controller namespace configuration"),
			"--controller-namespace must be non-empty")
		os.Exit(1)
	}
	if strings.TrimSpace(maasSubscriptionNamespace) == "" {
		setupLog.Error(stderrors.New("invalid MaaS subscription namespace configuration"),
			"--maas-subscription-namespace must be non-empty")
		os.Exit(1)
	}
	if strings.TrimSpace(aitenantNamespace) == "" {
		setupLog.Error(stderrors.New("invalid AITenant namespace configuration"),
			"--aitenant-namespace must be non-empty")
		os.Exit(1)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	cfg := ctrl.GetConfigOrDie()
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		setupLog.Error(err, "unable to create Kubernetes client for managed namespace setup")
		os.Exit(1)
	}
	if err := ensureSubscriptionNamespaceWithClient(context.Background(), maasSubscriptionNamespace, clientset); err != nil {
		setupLog.Error(err, "unable to ensure subscription namespace exists", "namespace", maasSubscriptionNamespace)
		os.Exit(1)
	}
	if err := ensureAITenantNamespaceWithClient(context.Background(), aitenantNamespace, clientset); err != nil {
		setupLog.Error(err, "unable to ensure AITenant namespace exists", "namespace", aitenantNamespace)
		os.Exit(1)
	}

	nsCfg := map[string]cache.Config{maasSubscriptionNamespace: {}}
	cacheOpts := cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			// Tenant CRs are watched cluster-wide to support AITenant-created tenants in any namespace.
			// TODO: Replace with proper namespace discovery from S1 when merged.
			&maasv1alpha1.Tenant{}:           {},
			&maasv1alpha1.MaaSAuthPolicy{}:   {Namespaces: nsCfg},
			&maasv1alpha1.MaaSSubscription{}: {Namespaces: nsCfg},
		},
	}
	setupLog.Info("watching namespace for MaaS CRs", "namespace", maasSubscriptionNamespace)
	if enableTenantNamespaceDiscovery {
		allNamespacesCfg := map[string]cache.Config{cache.AllNamespaces: {}}
		cacheOpts = cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&maasv1alpha1.Tenant{}:           {Namespaces: allNamespacesCfg},
				&maasv1alpha1.MaaSAuthPolicy{}:   {Namespaces: allNamespacesCfg},
				&maasv1alpha1.MaaSSubscription{}: {Namespaces: allNamespacesCfg},
			},
		}
		setupLog.Info("watching MaaS CRs across all namespaces for tenant discovery",
			"defaultNamespace", maasSubscriptionNamespace,
			"tenantNamespaceLabel", tenantreconcile.LabelAIGatewayTenant,
			"compatTenantNamespaceLabel", tenantreconcile.LabelManagedByAITenant)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cacheOpts,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "maas-controller.models-as-a-service.opendatahub.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Auto-detect cluster audience from OpenShift/ROSA; fall back to the standard Kubernetes audience.
	// Use GetAPIReader() instead of GetClient() because the cache hasn't started yet.
	clusterAudience := "https://kubernetes.default.svc"
	if detectedAudience, err := getClusterServiceAccountIssuer(mgr.GetAPIReader()); err == nil && detectedAudience != "" {
		setupLog.Info("auto-detected cluster service account issuer", "audience", detectedAudience)
		clusterAudience = detectedAudience
	} else if err != nil {
		setupLog.Error(err, "unable to auto-detect cluster service account issuer, using default", "default", clusterAudience)
	}

	if err := (&maas.MaaSModelRefReconciler{
		Client:                          mgr.GetClient(),
		Scheme:                          mgr.GetScheme(),
		GatewayName:                     gatewayName,
		GatewayNamespace:                gatewayNamespace,
		DefaultTenantNamespace:          maasSubscriptionNamespace,
		TenantNamespaceDiscoveryEnabled: enableTenantNamespaceDiscovery,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MaaSModelRef")
		os.Exit(1)
	}
	if err := (&maas.MaaSAuthPolicyReconciler{
		Client:                          mgr.GetClient(),
		Scheme:                          mgr.GetScheme(),
		MaaSAPINamespace:                maasAPINamespace,
		TenantNamespace:                 maasSubscriptionNamespace,
		GatewayName:                     gatewayName,
		GatewayNamespace:                gatewayNamespace,
		ClusterAudience:                 clusterAudience,
		MetadataCacheTTL:                metadataCacheTTL,
		AuthzCacheTTL:                   authzCacheTTL,
		TenantNamespaceDiscoveryEnabled: enableTenantNamespaceDiscovery,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MaaSAuthPolicy")
		os.Exit(1)
	}
	if err := (&maas.MaaSSubscriptionReconciler{
		Client:                          mgr.GetClient(),
		Scheme:                          mgr.GetScheme(),
		DefaultTenantNamespace:          maasSubscriptionNamespace,
		TenantNamespaceDiscoveryEnabled: enableTenantNamespaceDiscovery,
		GatewayName:                     gatewayName,
		GatewayNamespace:                gatewayNamespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MaaSSubscription")
		os.Exit(1)
	}
	if err := (&maas.AITenantReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		APIReader:         mgr.GetAPIReader(),
		AppNamespace:      maasAPINamespace,
		TenantNamespace:   maasSubscriptionNamespace,
		AITenantNamespace: aitenantNamespace,
		GatewayNamespace:  gatewayNamespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AITenant")
		os.Exit(1)
	}

	if err := (&externalmodel.Reconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		Log:              ctrl.Log.WithName("controllers").WithName("ExternalModel"),
		GatewayName:      gatewayName,
		GatewayNamespace: gatewayNamespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ExternalModel")
		os.Exit(1)
	}

	if err := mgr.Add(&managedNamespaceMonitor{
		clientset:          clientset,
		namespace:          maasSubscriptionNamespace,
		purpose:            "subscription",
		interval:           subscriptionNamespaceMaintainInterval,
		needLeaderElection: enableLeaderElection,
	}); err != nil {
		setupLog.Error(err, "unable to add subscription namespace monitor")
		os.Exit(1)
	}
	if err := mgr.Add(&managedNamespaceMonitor{
		clientset:          clientset,
		namespace:          aitenantNamespace,
		purpose:            "aitenant",
		interval:           subscriptionNamespaceMaintainInterval,
		needLeaderElection: enableLeaderElection,
	}); err != nil {
		setupLog.Error(err, "unable to add AITenant namespace monitor")
		os.Exit(1)
	}

	// Startup ordering contract:
	//   1. Managed namespace ensures run synchronously above, before the manager starts.
	//   2. LifecycleReconciler creates Config/default when maas-controller is running (see Setup below).
	//   3. ensureClusterBootstrapRunnable creates the default AITenant once Config/default exists (no owner refs).
	//   4. AITenant reconciler creates/adopts Tenant/default-tenant.
	//   5. LifecycleReconciler patches Config→AITenant/Tenant owner refs.
	//   6. If Tenant reconciles before Config exists, readyConfigOrWait requeues until the anchor appears.

	manifestPath := os.Getenv("MAAS_PLATFORM_MANIFESTS")
	if manifestPath == "" {
		manifestPath = tenantreconcile.DefaultManifestPath()
	}
	if abs, err := filepath.Abs(manifestPath); err == nil {
		manifestPath = abs
	}
	setupLog.Info("Tenant platform kustomize path", "path", manifestPath)

	if err := (&maas.TenantReconciler{
		Client:                          mgr.GetClient(),
		Scheme:                          mgr.GetScheme(),
		ManifestPath:                    manifestPath,
		AppNamespace:                    maasAPINamespace,
		TenantNamespace:                 maasSubscriptionNamespace,
		GatewayName:                     gatewayName,
		GatewayNamespace:                gatewayNamespace,
		ClusterAudience:                 clusterAudience,
		TenantNamespaceDiscoveryEnabled: enableTenantNamespaceDiscovery,
		MetadataCacheTTL:                metadataCacheTTL,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Tenant")
		os.Exit(1)
	}

	// LifecycleReconciler creates Config/default when maas-controller is running, links the
	// Deployment and default-tenant to Config (non-controller owner refs), and strips the legacy
	// cleanup finalizer if present. maasAPINamespace is where ODH deployed maas-controller.
	if err := (&maas.LifecycleReconciler{
		Client:                      mgr.GetClient(),
		Scheme:                      mgr.GetScheme(),
		DeploymentName:              "maas-controller",
		DeploymentNS:                controllerNamespace,
		TenantSubscriptionNamespace: maasSubscriptionNamespace,
		AITenantNamespace:           aitenantNamespace,
		ObservabilityManifestsPath:  observabilityManifestsPath,
		MonitoringNamespace:         monitoringNamespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "SelfDeployment")
		os.Exit(1)
	}

	// Setup validating webhooks for placement-sensitive MaaS resources.
	if err := (&webhook.AITenantValidator{
		Client:            mgr.GetAPIReader(),
		AITenantNamespace: aitenantNamespace,
		GatewayNamespace:  gatewayNamespace,
	}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "AITenant")
		os.Exit(1)
	}

	// MaaSSubscription and MaaSAuthPolicy must be created in tenant-enabled namespaces.
	// This prevents users from creating resources in random namespaces where they
	// would be silently ignored.
	tenantValidator := &webhook.TenantNamespaceValidator{
		Client: mgr.GetAPIReader(), // Use APIReader for uncached reads
	}

	if err := (&webhook.MaaSSubscriptionValidator{
		Client:    mgr.GetClient(),
		Validator: tenantValidator,
	}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "MaaSSubscription")
		os.Exit(1)
	}

	if err := (&webhook.MaaSAuthPolicyValidator{
		Client:    mgr.GetClient(),
		Validator: tenantValidator,
	}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "MaaSAuthPolicy")
		os.Exit(1)
	}

	if err := mgr.Add(ensureClusterBootstrapRunnable(mgr, maasSubscriptionNamespace, aitenantNamespace, controllerNamespace, "maas-controller", gatewayName, gatewayNamespace)); err != nil {
		setupLog.Error(err, "unable to register ensureClusterBootstrap runnable")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	// readyz: uncached Namespace GET each probe — see subscriptionNamespaceReadiness.
	if err := mgr.AddReadyzCheck("readyz", subscriptionNamespaceReadiness(clientset, maasSubscriptionNamespace)); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
