package tlsprofile

import (
	"fmt"
	"reflect"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// Watcher monitors the config.openshift.io/v1 APIServer resource for TLS
// profile changes and invokes the callback when a change is detected.
type Watcher struct {
	initial ProfileSpec
	factory dynamicinformer.DynamicSharedInformerFactory
}

// NewWatcher creates a Watcher that will invoke onChange when the cluster TLS
// profile diverges from initialProfile. Call Start to begin watching.
func NewWatcher(restConfig *rest.Config, initialProfile ProfileSpec, onChange func(oldProfile, newProfile ProfileSpec)) (*Watcher, error) {
	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynClient, 0)
	informer := factory.ForResource(apiServerGVR).Informer()

	if _, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(_, newObj any) {
			u, ok := newObj.(*unstructured.Unstructured)
			if !ok {
				return
			}
			current, err := parseProfileFromAPIServer(u)
			if err != nil {
				return
			}
			if !profileEqual(initialProfile, current) && onChange != nil {
				onChange(initialProfile, current)
			}
		},
	}); err != nil {
		return nil, fmt.Errorf("adding event handler: %w", err)
	}

	return &Watcher{initial: initialProfile, factory: factory}, nil
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
