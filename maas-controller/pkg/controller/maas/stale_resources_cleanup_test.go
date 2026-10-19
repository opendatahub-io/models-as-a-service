//nolint:testpackage
package maas

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"

	. "github.com/onsi/gomega"
)

func staleUsageObjects(namespace string) []*unstructured.Unstructured {
	dashboard := lifecycleTestUnstructured(schema.GroupVersionKind{Group: "perses.dev", Version: "v1alpha1", Kind: "PersesDashboard"}, namespace, "dashboard-3-maas-usage-admin")
	dashboard.Object["spec"] = map[string]any{"display": map[string]any{"name": "Usage"}}
	datasource := lifecycleTestUnstructured(schema.GroupVersionKind{Group: "perses.dev", Version: "v1alpha1", Kind: "PersesDatasource"}, namespace, staleUsageDatasourceName)
	datasource.Object["spec"] = map[string]any{"config": map[string]any{"plugin": map[string]any{
		"kind": "PrometheusDatasource",
		"spec": map[string]any{"proxy": map[string]any{"spec": map[string]any{
			"url": "https://thanos-querier.openshift-monitoring.svc:9092?namespace=kuadrant-system",
		}}},
	}}}
	ca := lifecycleTestUnstructured(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, namespace, "prometheus-web-tls-ca")
	return []*unstructured.Unstructured{dashboard, datasource, ca}
}

func TestCleanupStaleResources(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "metrics disabled"
		if enabled {
			name = "metrics enabled"
		}
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			stale := staleUsageObjects("redhat-ods-applications")
			otherNamespace := staleUsageObjects("other-applications")
			cfg := &maasv1alpha1.Config{ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: "config-uid"}, Spec: maasv1alpha1.ConfigSpec{UsageMetricsDashboard: ptr.To(enabled)}}
			dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "maas-controller", Namespace: "redhat-ods-applications"}}
			cl := fake.NewClientBuilder().WithScheme(lifecycleTestScheme(t)).WithStatusSubresource(cfg).WithObjects(dep, cfg, stale[0], stale[1], stale[2], otherNamespace[0], otherNamespace[1], otherNamespace[2]).Build()
			r := &LifecycleReconciler{Client: cl, DeploymentNS: "redhat-ods-applications"}
			// No monitoring namespace or manifest path: migration must still run.
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(dep)})
			g.Expect(err).NotTo(HaveOccurred())
			for _, obj := range stale {
				g.Expect(apierrors.IsNotFound(cl.Get(context.Background(), client.ObjectKeyFromObject(obj), obj.DeepCopy()))).To(BeTrue())
			}
			for _, obj := range otherNamespace {
				g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(obj), obj.DeepCopy())).To(Succeed())
			}
			_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(dep)})
			g.Expect(err).NotTo(HaveOccurred(), "repeated cleanup must be harmless")
		})
	}

	t.Run("preserves current and unrelated resources", func(t *testing.T) {
		g := NewWithT(t)
		current := staleUsageObjects("opendatahub")
		current[0].Object["spec"] = map[string]any{"display": map[string]any{"name": "Usage (legacy)"}}
		current[1].SetName("data-science-prometheus-datasource")
		current[2].SetName("current-ca")
		g.Expect(unstructured.SetNestedField(current[1].Object, "https://custom-prometheus.example", "spec", "config", "plugin", "spec", "proxy", "spec", "url")).To(Succeed())
		unrelated := staleUsageObjects("opendatahub")
		unrelated[0].SetName("custom-dashboard")
		unrelated[1].SetName("custom-datasource")
		unrelated[2].SetName("custom-ca")
		objects := []client.Object{current[0], current[1], current[2], unrelated[0], unrelated[1], unrelated[2]}
		cl := fake.NewClientBuilder().WithScheme(lifecycleTestScheme(t)).WithObjects(objects...).Build()
		r := &LifecycleReconciler{Client: cl, DeploymentNS: "opendatahub", MonitoringNamespace: "opendatahub"}
		g.Expect(r.cleanupStaleResources(context.Background(), ctrl.Log)).To(Succeed())
		for _, obj := range append(current, unrelated...) {
			g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(obj), obj.DeepCopy())).To(Succeed())
		}
	})

	t.Run("empty applications namespace does not query cluster", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect((&LifecycleReconciler{}).cleanupStaleResources(context.Background(), ctrl.Log)).To(Succeed())
	})
}

func TestCleanupStaleResourcesErrors(t *testing.T) {
	for _, tc := range []struct {
		name        string
		getError    error
		deleteError error
		wantError   bool
	}{
		{name: "missing CRD", getError: &apimeta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "perses.dev", Kind: "PersesDashboard"}}},
		{name: "read forbidden", getError: apierrors.NewForbidden(schema.GroupResource{Group: "perses.dev", Resource: "persesdashboards"}, "dashboard", errors.New("denied")), wantError: true},
		{name: "delete fails", deleteError: errors.New("delete failed"), wantError: true},
		{name: "concurrent deletion", deleteError: apierrors.NewNotFound(schema.GroupResource{Group: "perses.dev", Resource: "persesdashboards"}, "dashboard")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			stale := staleUsageObjects("apps")
			cl := fake.NewClientBuilder().WithScheme(lifecycleTestScheme(t)).WithObjects(stale[0], stale[1], stale[2]).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if tc.getError != nil {
						return tc.getError
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					options := (&client.DeleteOptions{}).ApplyOptions(opts)
					g.Expect(options.Preconditions).NotTo(BeNil())
					g.Expect(*options.Preconditions.UID).To(Equal(obj.GetUID()))
					g.Expect(*options.Preconditions.ResourceVersion).To(Equal(obj.GetResourceVersion()))
					if tc.deleteError != nil {
						return tc.deleteError
					}
					return c.Delete(ctx, obj, opts...)
				},
			}).Build()
			r := &LifecycleReconciler{Client: cl, DeploymentNS: "apps"}
			err := r.cleanupStaleResources(context.Background(), ctrl.Log)
			if tc.wantError {
				g.Expect(err).To(HaveOccurred())
			} else {
				g.Expect(err).NotTo(HaveOccurred())
			}
		})
	}
}

func TestStaleResourcesCleanupOnceAndRetry(t *testing.T) {
	for _, mode := range []string{"success", "read failure", "delete failure", "missing API"} {
		t.Run(mode, func(t *testing.T) {
			g := NewWithT(t)
			objects := staleUsageObjects("apps")
			var reads, deletes int
			first := true
			cl := fake.NewClientBuilder().WithScheme(lifecycleTestScheme(t)).WithObjects(objects[0], objects[1], objects[2]).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					reads++
					if first && mode == "read failure" {
						return errors.New("temporary read error")
					}
					if first && mode == "missing API" && obj.GetObjectKind().GroupVersionKind().Group == "perses.dev" {
						return &apimeta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "perses.dev", Kind: obj.GetObjectKind().GroupVersionKind().Kind}}
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					deletes++
					if first && mode == "delete failure" {
						return errors.New("temporary delete error")
					}
					return c.Delete(ctx, obj, opts...)
				},
			}).Build()
			r := &LifecycleReconciler{Client: cl, DeploymentNS: "apps"}
			err := r.cleanupStaleResources(context.Background(), ctrl.Log)
			if mode == "read failure" || mode == "delete failure" {
				g.Expect(err).To(HaveOccurred())
			} else {
				g.Expect(err).NotTo(HaveOccurred())
			}
			g.Expect(r.staleResourcesCleanupCompleted).To(Equal(mode == "success"))
			first = false
			g.Expect(r.cleanupStaleResources(context.Background(), ctrl.Log)).To(Succeed())
			g.Expect(r.staleResourcesCleanupCompleted).To(BeTrue())
			readsBefore, deletesBefore := reads, deletes
			g.Expect(r.cleanupStaleResources(context.Background(), ctrl.Log)).To(Succeed())
			g.Expect(reads).To(Equal(readsBefore), "completed cleanup must not read again")
			g.Expect(deletes).To(Equal(deletesBefore), "completed cleanup must not delete again")
			for _, obj := range objects {
				g.Expect(apierrors.IsNotFound(cl.Get(context.Background(), client.ObjectKeyFromObject(obj), obj.DeepCopy()))).To(BeTrue())
			}
		})
	}
}

func TestStaleResourcesCleanupConcurrent(t *testing.T) {
	g := NewWithT(t)
	objects := staleUsageObjects("apps")
	var reads, deletes atomic.Int32
	cl := fake.NewClientBuilder().WithScheme(lifecycleTestScheme(t)).WithObjects(objects[0], objects[1], objects[2]).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			reads.Add(1)
			return c.Get(ctx, key, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deletes.Add(1)
			return c.Delete(ctx, obj, opts...)
		},
	}).Build()
	r := &LifecycleReconciler{Client: cl, DeploymentNS: "apps"}
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Go(func() { errs <- r.cleanupStaleResources(context.Background(), ctrl.Log) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		g.Expect(err).NotTo(HaveOccurred())
	}
	g.Expect(reads.Load()).To(Equal(int32(3)))
	g.Expect(deletes.Load()).To(Equal(int32(3)))
}

func TestStaleUsageDashboardDisplayName(t *testing.T) {
	for _, name := range []string{"Usage", "Usage (legacy)", "Customized old usage", ""} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			objects := staleUsageObjects("apps")
			g.Expect(unstructured.SetNestedField(objects[0].Object, name, "spec", "display", "name")).To(Succeed())
			// The obsolete datasource is removed by name, regardless of its URL.
			g.Expect(unstructured.SetNestedField(objects[1].Object, "https://custom.example", "spec", "config", "plugin", "spec", "proxy", "spec", "url")).To(Succeed())
			cl := fake.NewClientBuilder().WithScheme(lifecycleTestScheme(t)).WithObjects(objects[0], objects[1], objects[2]).Build()
			r := &LifecycleReconciler{Client: cl, DeploymentNS: "apps", MonitoringNamespace: "apps"}
			g.Expect(r.cleanupStaleResources(context.Background(), ctrl.Log)).To(Succeed())
			err := cl.Get(context.Background(), client.ObjectKeyFromObject(objects[0]), objects[0].DeepCopy())
			if name == "Usage (legacy)" {
				g.Expect(err).NotTo(HaveOccurred())
			} else {
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}
			for _, obj := range objects[1:] {
				g.Expect(apierrors.IsNotFound(cl.Get(context.Background(), client.ObjectKeyFromObject(obj), obj.DeepCopy()))).To(BeTrue())
			}
		})
	}
}
