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
	"os"
	"time"

	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	corev1 "k8s.io/api/core/v1"
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
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/controller/maas"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/reconciler/externalmodel"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(kservev1alpha1.AddToScheme(scheme))
	utilruntime.Must(gatewayapiv1.Install(scheme))
	utilruntime.Must(maasv1alpha1.AddToScheme(scheme))
}

//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;create

// ensureSubscriptionNamespaceExists checks whether the subscription namespace exists
// and creates it if missing. It checks for existence first so that the controller can
// start even when the service account lacks namespace-create permission (common in
// operator-managed deployments where the operator pre-creates the namespace).
// Permanent errors such as Forbidden are not retried.
func ensureSubscriptionNamespaceExists(ctx context.Context, namespace string) error {
	cfg := ctrl.GetConfigOrDie()
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("unable to create Kubernetes client: %w", err)
	}

	_, err = clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err == nil {
		setupLog.Info("subscription namespace already exists", "namespace", namespace)
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
				},
			},
		}

		_, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
		if err == nil || errors.IsAlreadyExists(err) {
			setupLog.Info("subscription namespace ready", "namespace", namespace)
			return true, nil
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

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Ensure subscription namespace exists before starting controllers
	if err := ensureSubscriptionNamespaceExists(context.Background(), maasSubscriptionNamespace); err != nil {
		setupLog.Error(err, "unable to ensure subscription namespace exists", "namespace", maasSubscriptionNamespace)
		os.Exit(1)
	}

	setupLog.Info("watching namespace for MaaS AuthPolicy and MaaSSubscription", "namespace", maasSubscriptionNamespace)
	cacheOpts := cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&maasv1alpha1.MaaSAuthPolicy{}:   {Namespaces: map[string]cache.Config{maasSubscriptionNamespace: {}}},
			&maasv1alpha1.MaaSSubscription{}: {Namespaces: map[string]cache.Config{maasSubscriptionNamespace: {}}},
		},
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
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

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
