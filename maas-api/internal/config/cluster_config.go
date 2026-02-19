package config

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/models"
)

type ClusterConfig struct {
	ClientSet *kubernetes.Clientset

	ConfigMapLister      corev1listers.ConfigMapLister
	NamespaceLister      corev1listers.NamespaceLister
	ServiceAccountLister corev1listers.ServiceAccountLister

	// MaaSModelLister lists MaaSModel CRs from the informer cache for GET /v1/models.
	MaaSModelLister models.MaaSModelLister

	informersSynced []cache.InformerSynced
	startFuncs      []func(<-chan struct{})
}

// maasModelLister implements models.MaaSModelLister from a cache.GenericLister (informer-backed).
type maasModelLister struct {
	lister cache.GenericLister
}

func (m *maasModelLister) List(namespace string) ([]*unstructured.Unstructured, error) {
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
		if namespace != "" && u.GetNamespace() != namespace {
			continue
		}
		out = append(out, u)
	}
	return out, nil
}

func NewClusterConfig(namespace string, resyncPeriod time.Duration) (*ClusterConfig, error) {
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

	coreFactory := informers.NewSharedInformerFactory(clientset, resyncPeriod)
	coreFactoryNs := informers.NewSharedInformerFactoryWithOptions(clientset, resyncPeriod, informers.WithNamespace(namespace))

	cmInformer := coreFactoryNs.Core().V1().ConfigMaps()
	nsInformer := coreFactory.Core().V1().Namespaces()
	saInformer := coreFactory.Core().V1().ServiceAccounts()

	// MaaSModel informer (cached); watches all namespaces so we can list any namespace from cache.
	maasDynamicFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, resyncPeriod)
	maasGVR := models.GVR()
	maasInformer := maasDynamicFactory.ForResource(maasGVR)
	maasModelListerVal := &maasModelLister{lister: maasInformer.Lister()}

	return &ClusterConfig{
		ClientSet: clientset,

		ConfigMapLister:      cmInformer.Lister(),
		NamespaceLister:      nsInformer.Lister(),
		ServiceAccountLister: saInformer.Lister(),

		MaaSModelLister: maasModelListerVal,

		informersSynced: []cache.InformerSynced{
			cmInformer.Informer().HasSynced,
			nsInformer.Informer().HasSynced,
			saInformer.Informer().HasSynced,
			maasInformer.Informer().HasSynced,
		},
		startFuncs: []func(<-chan struct{}){
			coreFactory.Start,
			coreFactoryNs.Start,
			maasDynamicFactory.Start,
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
