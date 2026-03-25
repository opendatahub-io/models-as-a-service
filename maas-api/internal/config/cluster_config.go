package config

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/auth"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/models"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
)

type ClusterConfig struct {
	ClientSet *kubernetes.Clientset

	// MaaSModelRefLister lists MaaSModelRef CRs from the informer cache for GET /v1/models.
	MaaSModelRefLister models.MaaSModelRefLister

	// MaaSSubscriptionLister lists MaaSSubscription CRs from the informer cache for subscription selection.
	MaaSSubscriptionLister subscription.Lister

	// AdminChecker uses SubjectAccessReview to check if a user is an admin.
	// Admin is determined by RBAC: can user create maasauthpolicies in the configured MaaS namespace?
	AdminChecker *auth.SARAdminChecker

	informersSynced []cache.InformerSynced
	startFuncs      []func(<-chan struct{})
}

// maasModelRefLister implements models.MaaSModelRefLister from a cache.GenericLister (informer-backed).
type maasModelRefLister struct {
	lister cache.GenericLister
}

func (m *maasModelRefLister) List() ([]*unstructured.Unstructured, error) {
	objs, err := m.lister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	out := make([]*unstructured.Unstructured, 0, len(objs))
	for _, o := range objs {
		u, ok := o.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		// Return all MaaSModelRefs from all namespaces (no filtering)
		out = append(out, u)
	}
	return out, nil
}

// subscriptionLister implements subscription.Lister from a cache.GenericLister (informer-backed).
type subscriptionLister struct {
	lister cache.GenericLister
}

func (s *subscriptionLister) List() ([]*unstructured.Unstructured, error) {
	objs, err := s.lister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	out := make([]*unstructured.Unstructured, 0, len(objs))
	for _, o := range objs {
		u, ok := o.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		out = append(out, u)
	}
	return out, nil
}

func NewClusterConfig(_ string, subscriptionNamespace string, resyncPeriod time.Duration) (*ClusterConfig, error) {
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
	maasModelRefListerVal := &maasModelRefLister{lister: maasInformer.Lister()}

	// MaaSSubscription informer (cached); watches only the configured namespace for subscription selection.
	subscriptionDynamicFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynamicClient, resyncPeriod, subscriptionNamespace, nil)
	subscriptionGVR := subscription.GVR()
	subscriptionInformer := subscriptionDynamicFactory.ForResource(subscriptionGVR)
	maasSubscriptionListerVal := &subscriptionLister{lister: subscriptionInformer.Lister()}

	// SAR-based admin checker: uses SubjectAccessReview to check RBAC permissions.
	// Admin is determined by: can user create maasauthpolicies in the MaaS namespace?
	// This aligns with RBAC from opendatahub-operator#3301 which grants admin groups CRUD access to MaaS resources.
	adminCheckerVal := auth.NewSARAdminChecker(clientset, subscriptionNamespace)

	return &ClusterConfig{
		ClientSet: clientset,

		MaaSModelRefLister:     maasModelRefListerVal,
		MaaSSubscriptionLister: maasSubscriptionListerVal,
		AdminChecker:           adminCheckerVal,

		informersSynced: []cache.InformerSynced{
			maasInformer.Informer().HasSynced,
			subscriptionInformer.Informer().HasSynced,
		},
		startFuncs: []func(<-chan struct{}){
			maasDynamicFactory.Start,
			subscriptionDynamicFactory.Start,
		},
	}, nil
}

func (c *ClusterConfig) StartAndWaitForSync(stopCh <-chan struct{}) bool {
	for _, start := range c.startFuncs {
		start(stopCh)
	}
	return cache.WaitForCacheSync(stopCh, c.informersSynced...)
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
