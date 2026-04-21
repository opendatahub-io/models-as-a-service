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
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
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
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(extv1.AddToScheme(scheme))
	utilruntime.Must(kservev1alpha1.AddToScheme(scheme))
	utilruntime.Must(gatewayapiv1.Install(scheme))
	utilruntime.Must(maasv1alpha1.AddToScheme(scheme))
}

//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;create

// ensureSubscriptionNamespaceWithClient checks whether the subscription namespace exists
// and creates it if missing. It checks for existence first so that the controller can
// start even when the service account lacks namespace-create permission (common in
// operator-managed deployments where the operator pre-creates the namespace).
// Permanent errors such as Forbidden are not retried.
//
// Handles the edge case where the namespace is in Terminating phase during RHOAI
// reinstall/upgrade - waits for deletion to complete before attempting creation.
func ensureSubscriptionNamespaceWithClient(ctx context.Context, namespace string, clientset kubernetes.Interface) error {
	ns, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err == nil {
		if ns.Status.Phase == corev1.NamespaceTerminating {
			setupLog.Info("subscription namespace is terminating, waiting for deletion to complete",
				"namespace", namespace)

			pollErr := wait.PollUntilContextTimeout(ctx, 2*time.Second, 90*time.Second, true,
				func(ctx context.Context) (bool, error) {
					checkNs, pollErr := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
					if errors.IsNotFound(pollErr) {
						setupLog.Info("terminating namespace has been deleted", "namespace", namespace)
						return true, nil
					}
					if errors.IsForbidden(pollErr) {
						setupLog.Info("insufficient permissions to poll namespace deletion status, "+
							"assuming namespace is managed externally",
							"namespace", namespace, "error", pollErr)
						return true, nil
					}
					if pollErr != nil {
						return false, fmt.Errorf("error checking namespace status during deletion wait: %w", pollErr)
					}
					if checkNs.Status.Phase == corev1.NamespaceActive || checkNs.Status.Phase == "" {
						setupLog.Info("subscription namespace became active during deletion wait "+
							"(recreated by operator or external process)",
							"namespace", namespace)
						return true, nil
					}
					setupLog.V(1).Info("namespace still terminating, will retry",
						"namespace", namespace, "phase", checkNs.Status.Phase)
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
			setupLog.Info("subscription namespace already exists",
				"namespace", namespace, "phase", ns.Status.Phase)
			return nil
		}
	}

	if errors.IsForbidden(err) {
		setupLog.Info("insufficient permissions to check namespace existence, assuming it exists — "+
			"verify that the ClusterRoleBinding references the correct namespace for the controller ServiceAccount",
			"namespace", namespace, "error", err)
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("unable to check if namespace %q exists: %w", namespace, err)
	}

	setupLog.Info("subscription namespace not found, attempting to create it", "namespace", namespace)
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
			setupLog.Info("subscription namespace ready", "namespace", namespace)
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
				setupLog.Info("subscription namespace ready", "namespace", namespace)
				return true, nil
			}
			setupLog.Info("namespace already exists but is not ready, will retry",
				"namespace", namespace, "phase", existingNs.Status.Phase)
			return false, nil
		}
		if errors.IsForbidden(err) {
			return false, fmt.Errorf("service account lacks permission to create namespace %q — "+
				"either pre-create the namespace or grant 'create' on namespaces to the controller service account: %w",
				namespace, err)
		}
		setupLog.Info("retrying namespace creation", "namespace", namespace, "error", err)
		return false, nil // transient error, retry
	})
}

// resolveNamespaceAfterTerminationWait interprets the namespace GET after a successful termination poll.
// If fallThroughToCreate is true, the caller must assign the original finalErr to the outer GET error and
// continue into namespace creation. If fallThroughToCreate is false and the returned error is nil, the
// subscription namespace is already satisfied (Active or assumed external management).
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

// subscriptionNamespaceMonitor periodically re-runs ensureSubscriptionNamespaceWithClient so a namespace
// removed while the process is running can be recreated. When leader election is enabled, only the leader runs this.
type subscriptionNamespaceMonitor struct {
	clientset          kubernetes.Interface
	namespace          string
	interval           time.Duration
	needLeaderElection bool
}

func (m *subscriptionNamespaceMonitor) NeedLeaderElection() bool {
	return m.needLeaderElection
}

func (m *subscriptionNamespaceMonitor) Start(ctx context.Context) error {
	if m.interval <= 0 {
		return fmt.Errorf("subscription namespace maintain interval must be positive, got %v", m.interval)
	}
	run := func() {
		innerCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if err := ensureSubscriptionNamespaceWithClient(innerCtx, m.namespace, m.clientset); err != nil {
			// Keep running; the next tick will retry. Alerting on sustained failure is better done via
			// metrics (e.g. Prometheus counter) in a follow-up if product needs it.
			setupLog.Error(err, "subscription namespace maintenance failed", "namespace", m.namespace)
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

// ensureDefaultTenantRunnable returns a manager.Runnable that periodically ensures the
// default-tenant CR exists. If the Tenant is deleted (e.g. during testing or operator
// lifecycle), it will be recreated on the next tick.
func ensureDefaultTenantRunnable(mgr ctrl.Manager, tenantNamespace string) manager.RunnableFunc {
	return func(ctx context.Context) error {
		log := ctrl.Log.WithName("setup").WithName("ensureDefaultTenant")
		c := mgr.GetClient()

		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		ensure := func() {
			key := client.ObjectKey{Name: maasv1alpha1.TenantInstanceName, Namespace: tenantNamespace}
			var existing maasv1alpha1.Tenant
			if err := c.Get(ctx, key, &existing); err == nil {
				return
			} else if !errors.IsNotFound(err) {
				log.Error(err, "failed to check for default-tenant")
				return
			}

			tenant := &maasv1alpha1.Tenant{
				TypeMeta: metav1.TypeMeta{
					APIVersion: maasv1alpha1.GroupVersion.String(),
					Kind:       maasv1alpha1.TenantKind,
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      maasv1alpha1.TenantInstanceName,
					Namespace: tenantNamespace,
				},
			}
			tenantreconcile.EnsureTenantGatewayDefaults(tenant)

			if err := c.Create(ctx, tenant); err != nil {
				if errors.IsAlreadyExists(err) {
					return
				}
				log.Error(err, "failed to create default-tenant", "namespace", tenantNamespace)
				return
			}
			log.Info("created default-tenant", "namespace", tenantNamespace)
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
	var maasAPINamespace string
	var maasSubscriptionNamespace string
	var clusterAudience string
	var metadataCacheTTL int64
	var authzCacheTTL int64
	var subscriptionNamespaceMaintainInterval time.Duration

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager.")
	flag.StringVar(&gatewayName, "gateway-name", "maas-default-gateway", "The name of the Gateway resource to use for model HTTPRoutes.")
	flag.StringVar(&gatewayNamespace, "gateway-namespace", "openshift-ingress", "The namespace of the Gateway resource.")
	flag.StringVar(&maasAPINamespace, "maas-api-namespace", "opendatahub", "The namespace where maas-api service is deployed.")
	flag.StringVar(&maasSubscriptionNamespace, "maas-subscription-namespace", "models-as-a-service", "The namespace to watch for MaaS CRs.")
	flag.StringVar(&clusterAudience, "cluster-audience", "https://kubernetes.default.svc", "The OIDC audience of the cluster for TokenReview. HyperShift/ROSA clusters use a custom OIDC provider URL.")
	flag.Int64Var(&metadataCacheTTL, "metadata-cache-ttl", 60, "TTL in seconds for Authorino metadata HTTP caching (apiKeyValidation, subscription-info).")
	flag.Int64Var(&authzCacheTTL, "authz-cache-ttl", 60, "TTL in seconds for Authorino OPA authorization caching (auth-valid, subscription-valid, require-group-membership).")
	flag.DurationVar(&subscriptionNamespaceMaintainInterval, "subscription-namespace-maintain-interval", 30*time.Second,
		"How often to re-check the subscription namespace while the manager is running (recreate if deleted). "+
			"Larger values reduce apiserver load; smaller values detect external deletions sooner.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	cfg := ctrl.GetConfigOrDie()
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		setupLog.Error(err, "unable to create Kubernetes client for subscription namespace setup")
		os.Exit(1)
	}
	if err := ensureSubscriptionNamespaceWithClient(context.Background(), maasSubscriptionNamespace, clientset); err != nil {
		setupLog.Error(err, "unable to ensure subscription namespace exists", "namespace", maasSubscriptionNamespace)
		os.Exit(1)
	}

	setupLog.Info("watching namespace for MaaS CRs", "namespace", maasSubscriptionNamespace)
	nsCfg := map[string]cache.Config{maasSubscriptionNamespace: {}}
	cacheOpts := cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&maasv1alpha1.Tenant{}:           {Namespaces: nsCfg},
			&maasv1alpha1.MaaSAuthPolicy{}:   {Namespaces: nsCfg},
			&maasv1alpha1.MaaSSubscription{}: {Namespaces: nsCfg},
		},
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

	// Auto-detect cluster audience from OpenShift/ROSA if using default value
	// Use GetAPIReader() instead of GetClient() because the cache hasn't started yet
	if clusterAudience == "https://kubernetes.default.svc" {
		if detectedAudience, err := getClusterServiceAccountIssuer(mgr.GetAPIReader()); err == nil && detectedAudience != "" {
			setupLog.Info("auto-detected cluster service account issuer", "audience", detectedAudience)
			clusterAudience = detectedAudience
		} else if err != nil {
			setupLog.Info("unable to auto-detect cluster service account issuer, using default", "error", err, "default", clusterAudience)
		}
	}

	if err := (&maas.MaaSModelRefReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		GatewayName:      gatewayName,
		GatewayNamespace: gatewayNamespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MaaSModelRef")
		os.Exit(1)
	}
	if err := (&maas.MaaSAuthPolicyReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		MaaSAPINamespace: maasAPINamespace,
		TenantNamespace:  maasSubscriptionNamespace,
		GatewayName:      gatewayName,
		ClusterAudience:  clusterAudience,
		MetadataCacheTTL: metadataCacheTTL,
		AuthzCacheTTL:    authzCacheTTL,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MaaSAuthPolicy")
		os.Exit(1)
	}
	if err := (&maas.MaaSSubscriptionReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MaaSSubscription")
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

	if err := mgr.Add(&subscriptionNamespaceMonitor{
		clientset:          clientset,
		namespace:          maasSubscriptionNamespace,
		interval:           subscriptionNamespaceMaintainInterval,
		needLeaderElection: enableLeaderElection,
	}); err != nil {
		setupLog.Error(err, "unable to add subscription namespace monitor")
		os.Exit(1)
	}

	// Ensure the default-tenant CR exists in the MaaS subscription namespace
	// (same namespace as MaaSSubscription / MaaSAuthPolicy CRs).
	// maas-controller owns creation; ODH operator only reads status and deletes on disable.
	if err := mgr.Add(ensureDefaultTenantRunnable(mgr, maasSubscriptionNamespace)); err != nil {
		setupLog.Error(err, "unable to register ensureDefaultTenant runnable")
		os.Exit(1)
	}

	manifestPath := os.Getenv("MAAS_PLATFORM_MANIFESTS")
	if manifestPath == "" {
		manifestPath = tenantreconcile.DefaultManifestPath()
	}
	if abs, err := filepath.Abs(manifestPath); err == nil {
		manifestPath = abs
	}
	setupLog.Info("Tenant platform kustomize path", "path", manifestPath)

	if err := (&maas.TenantReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		ManifestPath:    manifestPath,
		AppNamespace:    maasAPINamespace,
		TenantNamespace: maasSubscriptionNamespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Tenant")
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
