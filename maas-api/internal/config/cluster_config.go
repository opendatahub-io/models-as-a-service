package config

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/auth"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/authpolicy"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/models"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
)

// infoLogger interface for logging (matches logger.Logger methods we need).
type infoLogger interface {
	Info(msg string, keysAndValues ...any)
}

type ClusterConfig struct {
	ClientSet     *kubernetes.Clientset
	DynamicClient dynamic.Interface

	// MaaSModelRefLister lists MaaSModelRef CRs from the informer cache for GET /v1/models.
	MaaSModelRefLister models.MaaSModelRefLister

	// MaaSSubscriptionLister lists MaaSSubscription CRs from the informer cache for subscription selection.
	MaaSSubscriptionLister subscription.Lister

	// MaaSAuthPolicyLister lists MaaSAuthPolicy CRs from the informer cache for model access checks.
	MaaSAuthPolicyLister authpolicy.Lister

	// AdminChecker uses SubjectAccessReview to check if a user is an admin.
	// Admin is determined by RBAC: can user create maasauthpolicies in the configured MaaS namespace?
	// Results are cached with a TTL to reduce Kubernetes API server load.
	AdminChecker *auth.CachedAdminChecker

	restConfig      *rest.Config
	informersSynced []cache.InformerSynced
	startFuncs      []func(<-chan struct{})
	log             infoLogger
}

// RESTConfig returns the Kubernetes REST config used by this cluster config.
func (c *ClusterConfig) RESTConfig() *rest.Config {
	return c.restConfig
}

// unstructuredLister wraps a cache.GenericLister and implements the List() method
// shared by models.MaaSModelRefLister, subscription.Lister, and authpolicy.Lister.
type unstructuredLister struct {
	lister cache.GenericLister
	log    infoLogger
}

func (u *unstructuredLister) List() ([]*unstructured.Unstructured, error) {
	objs, err := u.lister.List(labels.Everything())
	if err != nil {
		if u.log != nil {
			u.log.Info("List() error", "error", err)
		}
		return nil, err
	}
	out := make([]*unstructured.Unstructured, 0, len(objs))
	for _, o := range objs {
		item, ok := o.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		out = append(out, item)
	}
	return out, nil
}

func NewClusterConfig(
	_ string, subscriptionNamespace string, resyncPeriod time.Duration,
	sarCacheMaxSize int, metricsRegisterer prometheus.Registerer, log infoLogger,
) (*ClusterConfig, error) {
	restConfig, err := LoadRestConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes clientset: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	// MaaSModelRef informer (cached); watches all namespaces so we can list any namespace from cache.
	maasDynamicFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, resyncPeriod)
	maasGVR := models.GVR()
	maasInformer := maasDynamicFactory.ForResource(maasGVR)
	maasModelRefListerVal := &unstructuredLister{lister: maasInformer.Lister(), log: log}
	log.Info("Created MaaSModelRef informer", "watchNamespace", "ALL", "gvr", maasGVR.String())

	// MaaSSubscription informer (cached); watches only the configured namespace for subscription selection.
	subscriptionDynamicFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynamicClient, resyncPeriod, subscriptionNamespace, nil)
	subscriptionGVR := subscription.GVR()
	subscriptionInformer := subscriptionDynamicFactory.ForResource(subscriptionGVR)
	maasSubscriptionListerVal := &unstructuredLister{lister: subscriptionInformer.Lister(), log: log}
	log.Info("Created MaaSSubscription informer", "watchNamespace", subscriptionNamespace, "gvr", subscriptionGVR.String())

	// MaaSAuthPolicy informer (cached); watches the subscription namespace for model access checks.
	authPolicyDynamicFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynamicClient, resyncPeriod, subscriptionNamespace, nil)
	authPolicyGVR := authpolicy.GVR()
	authPolicyInformer := authPolicyDynamicFactory.ForResource(authPolicyGVR)
	authPolicyListerVal := &unstructuredLister{lister: authPolicyInformer.Lister(), log: log}

	// SAR-based admin checker: uses SubjectAccessReview to check RBAC permissions.
	// Admin is determined by: can user create maasauthpolicies in the MaaS namespace?
	// This aligns with RBAC from opendatahub-operator#3301 which grants admin groups CRUD access to MaaS resources.
	// Results are cached for 30s to reduce K8s API server load under high traffic.
	sarChecker := auth.NewSARAdminChecker(clientset, subscriptionNamespace)
	adminCheckerVal := auth.NewCachedAdminChecker(sarChecker, 30*time.Second, 2*time.Second, sarCacheMaxSize, metricsRegisterer, nil)

	return &ClusterConfig{
		ClientSet:     clientset,
		DynamicClient: dynamicClient,

		MaaSModelRefLister:     maasModelRefListerVal,
		MaaSSubscriptionLister: maasSubscriptionListerVal,
		MaaSAuthPolicyLister:   authPolicyListerVal,
		AdminChecker:           adminCheckerVal,

		restConfig: restConfig,
		informersSynced: []cache.InformerSynced{
			maasInformer.Informer().HasSynced,
			subscriptionInformer.Informer().HasSynced,
			authPolicyInformer.Informer().HasSynced,
		},
		startFuncs: []func(<-chan struct{}){
			maasDynamicFactory.Start,
			subscriptionDynamicFactory.Start,
			authPolicyDynamicFactory.Start,
		},
		log: log,
	}, nil
}

func (c *ClusterConfig) StartAndWaitForSync(stopCh <-chan struct{}) bool {
	for _, start := range c.startFuncs {
		start(stopCh)
	}
	return cache.WaitForCacheSync(stopCh, c.informersSynced...)
}

// ResolveGatewayInternalHost finds the cluster-internal DNS name of the gateway's
// Service by looking up Services labeled with the standard Gateway API label
// gateway.networking.k8s.io/gateway-name=<gatewayName> in gatewayNamespace.
// Only Services owned by a Gateway resource (via ownerReferences) and exposing
// port 443 are considered. Returns "<service-name>.<namespace>.svc.cluster.local".
func ResolveGatewayInternalHost(ctx context.Context, clientset kubernetes.Interface, gatewayName, gatewayNamespace string) (string, error) {
	labelSelector := fmt.Sprintf("gateway.networking.k8s.io/gateway-name=%s", gatewayName)
	svcs, err := clientset.CoreV1().Services(gatewayNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list services in %s with label %s: %w", gatewayNamespace, labelSelector, err)
	}

	var candidates []string
	for _, svc := range svcs.Items {
		ownedByGateway := false
		for _, ref := range svc.OwnerReferences {
			if ref.Kind == "Gateway" && ref.Name == gatewayName &&
				strings.HasPrefix(ref.APIVersion, "gateway.networking.k8s.io/") {
				ownedByGateway = true
				break
			}
		}
		if !ownedByGateway {
			continue
		}
		for _, port := range svc.Spec.Ports {
			if port.Port == 443 {
				candidates = append(candidates, svc.Name)
				break
			}
		}
	}

	switch len(candidates) {
	case 0:
		// No gateway service found - this is expected for test gateways or gateways
		// without proper infrastructure. Return empty string to allow startup.
		// Model access checks will be disabled.
		return "", nil
	case 1:
		return fmt.Sprintf("%s.%s.svc.cluster.local", candidates[0], gatewayNamespace), nil
	default:
		return "", fmt.Errorf("expected 1 gateway service for %s/%s, found %d: %v", gatewayNamespace, gatewayName, len(candidates), candidates)
	}
}

// LoadRestConfig creates a *rest.Config using client-go loading rules.
// Order:
// 1) KUBECONFIG or $HOME/.kube/config (if present and non-default)
// 2) If kubeconfig is empty/default (or IsEmptyConfig), fall back to in-cluster
// Note: if kubeconfig is set but invalid (non-empty error), the error is returned.
func LoadRestConfig() (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}

	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	config, kubeconfigErr := kubeConfig.ClientConfig()
	if kubeconfigErr != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", kubeconfigErr)
	}

	return config, nil
}
