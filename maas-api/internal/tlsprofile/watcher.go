package tlsprofile

import (
	"errors"
	"fmt"
	"reflect"

	confv1 "github.com/openshift/api/config/v1"
	configclientset "github.com/openshift/client-go/config/clientset/versioned"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// Watcher monitors the config.openshift.io/v1 APIServer resource for TLS
// profile or adherence changes and invokes the callback when either changes.
type Watcher struct {
	factory configinformers.SharedInformerFactory
}

// NewWatcher creates a Watcher that will invoke onChange when the cluster TLS
// settings diverge from initialSettings. Call Start to begin watching.
func NewWatcher(restConfig *rest.Config, initialSettings Settings, onChange func(oldSettings, newSettings Settings)) (*Watcher, error) {
	if restConfig == nil {
		return nil, errors.New("restConfig must not be nil")
	}

	configClient, err := configclientset.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	factory := configinformers.NewSharedInformerFactory(configClient, 0)
	informer := factory.Config().V1().APIServers().Informer()
	handleAPIServer := settingsEventHandler(initialSettings, onChange)

	if _, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: handleAPIServer,
		UpdateFunc: func(_, newObj any) {
			handleAPIServer(newObj)
		},
	}); err != nil {
		return nil, fmt.Errorf("adding event handler: %w", err)
	}

	return &Watcher{factory: factory}, nil
}

func settingsEventHandler(initial Settings, onChange func(oldSettings, newSettings Settings)) func(any) {
	return func(obj any) {
		apiServer, ok := obj.(*confv1.APIServer)
		if !ok {
			return
		}
		if apiServer.Name != "cluster" {
			return
		}
		current, err := settingsFromAPIServer(apiServer)
		if err != nil {
			return
		}
		if !settingsEqual(initial, current) && onChange != nil {
			onChange(initial, current)
		}
	}
}

// Start begins watching and blocks until stopCh is closed.
// Returns an error if the informer cache fails to sync.
func (w *Watcher) Start(stopCh <-chan struct{}) error {
	w.factory.Start(stopCh)
	synced := w.factory.WaitForCacheSync(stopCh)
	for gvr, ok := range synced {
		if !ok {
			return fmt.Errorf("informer cache sync failed for %s", gvr.String())
		}
	}
	<-stopCh
	return nil
}

func profileEqual(a, b ProfileSpec) bool {
	return a.Type == b.Type &&
		a.MinTLSVersion == b.MinTLSVersion &&
		reflect.DeepEqual(a.Ciphers, b.Ciphers)
}

func settingsEqual(a, b Settings) bool {
	return a.Adherence == b.Adherence && profileEqual(a.Profile, b.Profile)
}
