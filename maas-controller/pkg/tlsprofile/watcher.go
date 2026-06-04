package tlsprofile

import (
	"context"
	"reflect"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// SecurityProfileWatcher watches the config.openshift.io/v1 APIServer resource
// for TLS profile changes and invokes OnProfileChange when a change is detected.
// Typical usage is to cancel the manager context so the operator restarts with
// the new profile.
type SecurityProfileWatcher struct {
	Client          client.Client
	Log             logr.Logger
	InitialProfile  ProfileSpec
	OnProfileChange func(oldProfile, newProfile ProfileSpec)
}

func (w *SecurityProfileWatcher) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	current, err := FetchAPIServerTLSProfile(ctx, w.Client)
	if err != nil {
		w.Log.Error(err, "failed to fetch TLS profile during watch reconcile, will retry")
		return reconcile.Result{}, err
	}

	if !profileEqual(w.InitialProfile, current) {
		w.Log.Info("TLS security profile changed",
			"oldType", w.InitialProfile.Type, "oldMinTLS", w.InitialProfile.MinTLSVersion,
			"newType", current.Type, "newMinTLS", current.MinTLSVersion,
		)
		if w.OnProfileChange != nil {
			w.OnProfileChange(w.InitialProfile, current)
		}
	}

	return reconcile.Result{}, nil
}

// SetupWithManager registers the watcher controller.
func (w *SecurityProfileWatcher) SetupWithManager(mgr manager.Manager) error {
	apiServerObj := &unstructured.Unstructured{}
	apiServerObj.SetGroupVersionKind(apiServerGVK)

	return ctrl.NewControllerManagedBy(mgr).
		Named("tls-profile-watcher").
		For(apiServerObj).
		Complete(w)
}

func profileEqual(a, b ProfileSpec) bool {
	return a.Type == b.Type &&
		a.MinTLSVersion == b.MinTLSVersion &&
		reflect.DeepEqual(a.Ciphers, b.Ciphers)
}
