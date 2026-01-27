package config

import (
	"fmt"
	"time"

	kserveclient "github.com/kserve/kserve/pkg/client/clientset/versioned"
	kserveinformers "github.com/kserve/kserve/pkg/client/informers/externalversions"
	kservelistersv1alpha1 "github.com/kserve/kserve/pkg/client/listers/serving/v1alpha1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	gatewayinformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"
	gatewaylisters "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"
)

type ClusterConfig struct {
	ClientSet *kubernetes.Clientset

	ConfigMapLister      corev1listers.ConfigMapLister
	NamespaceLister      corev1listers.NamespaceLister
	ServiceAccountLister corev1listers.ServiceAccountLister

	LLMInferenceServiceLister kservelistersv1alpha1.LLMInferenceServiceLister

	HTTPRouteLister gatewaylisters.HTTPRouteLister

	informersSynced []cache.InformerSynced
	startFuncs      []func(<-chan struct{})
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

	kserveClientset, err := kserveclient.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create KServe clientset: %w", err)
	}

	gatewayClientset, err := gatewayclient.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create Gateway API clientset: %w", err)
	}

	coreFactory := informers.NewSharedInformerFactory(clientset, resyncPeriod)
	coreFactoryNs := informers.NewSharedInformerFactoryWithOptions(clientset, resyncPeriod, informers.WithNamespace(namespace))
	kserveFactory := kserveinformers.NewSharedInformerFactory(kserveClientset, resyncPeriod)
	gatewayFactory := gatewayinformers.NewSharedInformerFactory(gatewayClientset, resyncPeriod)

	cmInformer := coreFactoryNs.Core().V1().ConfigMaps()
	nsInformer := coreFactory.Core().V1().Namespaces()
	saInformer := coreFactory.Core().V1().ServiceAccounts()
	llmIsvcInformer := kserveFactory.Serving().V1alpha1().LLMInferenceServices()
	httpRouteInformer := gatewayFactory.Gateway().V1().HTTPRoutes()

	return &ClusterConfig{
		ClientSet: clientset,

		ConfigMapLister:      cmInformer.Lister(),
		NamespaceLister:      nsInformer.Lister(),
		ServiceAccountLister: saInformer.Lister(),

		LLMInferenceServiceLister: llmIsvcInformer.Lister(),

		HTTPRouteLister: httpRouteInformer.Lister(),

		informersSynced: []cache.InformerSynced{
			cmInformer.Informer().HasSynced,
			nsInformer.Informer().HasSynced,
			saInformer.Informer().HasSynced,
			llmIsvcInformer.Informer().HasSynced,
			httpRouteInformer.Informer().HasSynced,
		},
		startFuncs: []func(<-chan struct{}){
			coreFactory.Start,
			coreFactoryNs.Start,
			kserveFactory.Start,
			gatewayFactory.Start,
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
