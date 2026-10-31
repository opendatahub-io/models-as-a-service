//nolint:testpackage
package maas

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"

	. "github.com/onsi/gomega"
)

const authorinoTestManifestsPath = "../../../../deployment/base/observability"

func TestAuthorinoMonitoringGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled *bool
	}{
		{name: "default"}, {name: "enabled", enabled: ptr.To(true)}, {name: "disabled", enabled: ptr.To(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()
			s := lifecycleTestScheme(t)
			cfg := &maasv1alpha1.Config{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "config-uid"}, Spec: maasv1alpha1.ConfigSpec{AuthorinoMonitoring: tc.enabled}}
			cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cfg).Build()
			r := &LifecycleReconciler{Client: cl, Scheme: s, AuthorinoMonitoringManifestsPath: authorinoTestManifestsPath, MonitoringNamespace: "product-monitoring"}
			resources, err := authorinoMonitoringResources(authorinoTestManifestsPath, r.MonitoringNamespace)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(resources).To(HaveLen(3))
			g.Expect(r.ensureAuthorinoMonitoring(ctx, logr.Discard())).To(Succeed())
			for _, resource := range resources {
				obj := resource.DeepCopy()
				err := cl.Get(ctx, client.ObjectKeyFromObject(obj), obj)
				if !ptr.Deref(tc.enabled, true) {
					g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
					continue
				}
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(metav1.IsControlledBy(obj, cfg)).To(BeTrue())
			}
			// Disabling cleans up all managed resources and is safe to repeat.
			g.Expect(cl.Get(ctx, client.ObjectKeyFromObject(cfg), cfg)).To(Succeed())
			cfg.Spec.AuthorinoMonitoring = ptr.To(false)
			g.Expect(cl.Update(ctx, cfg)).To(Succeed())
			g.Expect(r.ensureAuthorinoMonitoring(ctx, logr.Discard())).To(Succeed())
			g.Expect(r.ensureAuthorinoMonitoring(ctx, logr.Discard())).To(Succeed())
			for _, resource := range resources {
				obj := resource.DeepCopy()
				g.Expect(apierrors.IsNotFound(cl.Get(ctx, client.ObjectKeyFromObject(obj), obj))).To(BeTrue())
			}
			// Re-enabling recreates resources, without requiring a controller restart.
			g.Expect(cl.Get(ctx, client.ObjectKeyFromObject(cfg), cfg)).To(Succeed())
			cfg.Spec.AuthorinoMonitoring = ptr.To(true)
			g.Expect(cl.Update(ctx, cfg)).To(Succeed())
			g.Expect(r.ensureAuthorinoMonitoring(ctx, logr.Discard())).To(Succeed())
			for _, resource := range resources {
				obj := resource.DeepCopy()
				g.Expect(cl.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
			}
		})
	}
}

func TestAuthorinoMonitoringPreservesUnownedResources(t *testing.T) {
	g := NewWithT(t)
	s := lifecycleTestScheme(t)
	cfg := &maasv1alpha1.Config{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "config-uid"}, Spec: maasv1alpha1.ConfigSpec{AuthorinoMonitoring: ptr.To(false)}}
	resources, err := authorinoMonitoringResources(authorinoTestManifestsPath, "product-monitoring")
	g.Expect(err).NotTo(HaveOccurred())
	objects := []client.Object{cfg}
	for _, resource := range resources {
		objects = append(objects, resource.DeepCopy())
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).Build()
	r := &LifecycleReconciler{Client: cl, Scheme: s, AuthorinoMonitoringManifestsPath: authorinoTestManifestsPath, MonitoringNamespace: "product-monitoring"}
	g.Expect(r.ensureAuthorinoMonitoring(context.Background(), logr.Discard())).To(Succeed())
	for _, resource := range resources {
		obj := resource.DeepCopy()
		g.Expect(cl.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)).To(Succeed())
		g.Expect(obj.GetOwnerReferences()).To(BeEmpty())
	}
}

func TestAuthorinoMonitoringApplyErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		applyError error
		apiVersion string
		wantError  bool
	}{
		{name: "missing API", applyError: &apimeta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "monitoring.rhobs", Kind: "PrometheusRule"}}},
		{name: "API endpoint not found", applyError: apierrors.NewNotFound(schema.GroupResource{Group: "monitoring.rhobs", Resource: "prometheusrules"}, "")},
		{name: "missing core API", apiVersion: "monitoring.coreos.com/v1", applyError: &apimeta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "monitoring.coreos.com", Kind: "ServiceMonitor"}}, wantError: true},
		{name: "forbidden", applyError: apierrors.NewForbidden(schema.GroupResource{Group: "monitoring.rhobs", Resource: "prometheusrules"}, "", errors.New("denied")), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			s := lifecycleTestScheme(t)
			cfg := &maasv1alpha1.Config{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "config-uid"}}
			cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cfg).WithInterceptorFuncs(interceptor.Funcs{
				Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
					version, ok := obj.(interface{ GetAPIVersion() string })
					g.Expect(ok).To(BeTrue())
					targetVersion := tc.apiVersion
					if targetVersion == "" {
						targetVersion = "monitoring.rhobs/v1"
					}
					if version.GetAPIVersion() == targetVersion {
						return tc.applyError
					}
					return nil
				},
			}).Build()
			r := &LifecycleReconciler{Client: cl, Scheme: s, AuthorinoMonitoringManifestsPath: authorinoTestManifestsPath, MonitoringNamespace: "product-monitoring"}
			err := r.ensureAuthorinoMonitoring(context.Background(), logr.Discard())
			if tc.wantError {
				g.Expect(err).To(MatchError(ContainSubstring(tc.applyError.Error())))
			} else {
				g.Expect(err).NotTo(HaveOccurred())
			}
		})
	}
}

func TestAuthorinoMonitoringProductResources(t *testing.T) {
	g := NewWithT(t)
	resources, err := authorinoMonitoringResources(authorinoTestManifestsPath, "product-monitoring")
	g.Expect(err).NotTo(HaveOccurred())
	alerts := map[string]string{}
	for _, res := range resources {
		g.Expect(res.GetNamespace()).To(Equal("product-monitoring"))
		if res.GetKind() == "ServiceMonitor" {
			g.Expect(res.GetAPIVersion()).To(Equal("monitoring.coreos.com/v1"))
			g.Expect(res.GetLabels()["monitoring.opendatahub.io/scrape"]).To(Equal("true"))
			namespaces, _, err := unstructured.NestedStringSlice(res.Object, "spec", "namespaceSelector", "matchNames")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(namespaces).To(ConsistOf("kuadrant-system", "rh-connectivity-link"))
			endpoints, _, err := unstructured.NestedSlice(res.Object, "spec", "endpoints")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(endpoints).To(HaveLen(1))
			g.Expect(endpoints[0]).To(HaveKeyWithValue("path", "/server-metrics"))
			continue
		}
		g.Expect(res.GetAPIVersion()).To(Equal("monitoring.rhobs/v1"))
		groups, _, err := unstructured.NestedSlice(res.Object, "spec", "groups")
		g.Expect(err).NotTo(HaveOccurred())
		for _, group := range groups {
			groupMap, ok := group.(map[string]any)
			g.Expect(ok).To(BeTrue())
			rules, found, err := unstructured.NestedSlice(groupMap, "rules")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(BeTrue())
			for _, rule := range rules {
				fields, ok := rule.(map[string]any)
				g.Expect(ok).To(BeTrue())
				g.Expect(fields["for"]).To(Equal("5m"))
				name, ok := fields["alert"].(string)
				g.Expect(ok).To(BeTrue())
				expr, ok := fields["expr"].(string)
				g.Expect(ok).To(BeTrue())
				alerts[name] = expr
			}
		}
	}
	g.Expect(alerts).To(HaveLen(3))
	g.Expect(alerts["MaaSAuthorinoAuthenticationHighFailureRate"]).To(ContainSubstring("auth_server_authconfig_response_status_total{"))
	g.Expect(alerts["MaaSAuthorinoAuthenticationHighFailureRate"]).To(ContainSubstring(") > 0.10"))
	g.Expect(alerts["MaaSAuthorinoAuthenticationHighLatency"]).To(ContainSubstring(") > 2"))
	g.Expect(alerts["MaaSAuthorinoMetadataEvaluatorHighFailureRate"]).To(ContainSubstring("auth_server_evaluator_cancelled_total{"))
	g.Expect(alerts["MaaSAuthorinoMetadataEvaluatorHighFailureRate"]).To(ContainSubstring(") > 0.1"))
}
