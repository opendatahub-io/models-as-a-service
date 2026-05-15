package tlsprofile

import (
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

	_, _ = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
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
	})

	return &Watcher{initial: initialProfile, factory: factory}, nil
}

// Start begins watching. The informer stops when stopCh is closed.
func (w *Watcher) Start(stopCh <-chan struct{}) {
	w.factory.Start(stopCh)
	w.factory.WaitForCacheSync(stopCh)
}

func profileEqual(a, b ProfileSpec) bool {
	return a.Type == b.Type &&
		a.MinTLSVersion == b.MinTLSVersion &&
		reflect.DeepEqual(a.Ciphers, b.Ciphers)
}
