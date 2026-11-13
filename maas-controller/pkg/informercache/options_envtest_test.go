/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package informercache_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/informercache"
	pkgtest "github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestCacheScopingEnvtest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Cache Scoping Envtest Suite")
}

var envTest *pkgtest.Client

// gatewayAPICRDPath finds the Gateway API CRD directory in the Go module cache.
func gatewayAPICRDPath() string {
	out, err := exec.CommandContext(context.Background(), "go", "list", "-m", "-f", "{{.Dir}}", "sigs.k8s.io/gateway-api").Output()
	if err != nil {
		return ""
	}
	return filepath.Join(strings.TrimSpace(string(out)), "config", "crd", "standard")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	gwCRDPath := gatewayAPICRDPath()
	Expect(gwCRDPath).NotTo(BeEmpty(), "cannot locate gateway-api CRD path in module cache")

	envTest = pkgtest.Configure(
		pkgtest.WithScheme(clientgoscheme.AddToScheme, maasv1alpha1.AddToScheme, gatewayapiv1.Install),
		pkgtest.WithCRDs(
			filepath.Join(pkgtest.ProjectRoot(), "deployment", "base", "maas-controller", "crd", "bases"),
			gwCRDPath,
		),
	).Start()

	for _, ns := range []string{"controller-ns", "infra-ns", "monitoring-ns", "aitenant-ns", "gateway-ns", "subscription-ns", "out-of-scope-ns"} {
		Expect(envTest.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())
	}
})

var _ = Describe("BuildCacheOptions scoping", func() {
	var (
		scopedCache cache.Cache
		ctx         context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		opts := informercache.BuildCacheOptions(informercache.Params{
			ControllerNamespace:   "controller-ns",
			InfraNamespace:        "infra-ns",
			MonitoringNamespace:   "monitoring-ns",
			AITenantNamespace:     "aitenant-ns",
			GatewayNamespace:      "gateway-ns",
			SubscriptionNamespace: "subscription-ns",
			EnableTenantDiscovery: false,
			SubscriptionNSExists:  true,
		})
		opts.Scheme = envTest.Environment.Scheme
		var err error
		scopedCache, err = cache.New(envTest.Config, opts)
		Expect(err).NotTo(HaveOccurred())

		cacheCtx, cancel := context.WithCancel(ctx)
		go func() { _ = scopedCache.Start(cacheCtx) }()
		Expect(scopedCache.WaitForCacheSync(cacheCtx)).To(BeTrue())
		DeferCleanup(cancel)
	})

	It("reads a ConfigMap in a scoped namespace", func() {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "test-cm", Namespace: "infra-ns"},
			Data:       map[string]string{"key": "value"},
		}
		Expect(envTest.Create(ctx, cm)).To(Succeed())
		DeferCleanup(func() { _ = envTest.Delete(ctx, cm) })

		got := &corev1.ConfigMap{}
		Expect(scopedCache.Get(ctx, client.ObjectKeyFromObject(cm), got)).To(Succeed())
		Expect(got.Data["key"]).To(Equal("value"))
	})

	It("rejects a ConfigMap read from an out-of-scope namespace", func() {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "test-cm-oos", Namespace: "out-of-scope-ns"},
			Data:       map[string]string{"key": "value"},
		}
		Expect(envTest.Create(ctx, cm)).To(Succeed())
		DeferCleanup(func() { _ = envTest.Delete(ctx, cm) })

		got := &corev1.ConfigMap{}
		err := scopedCache.Get(ctx, client.ObjectKeyFromObject(cm), got)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("unknown namespace"))
	})

	It("reads a Deployment in the gateway namespace", func() {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "test-deploy", Namespace: "gateway-ns"},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
				},
			},
		}
		Expect(envTest.Create(ctx, dep)).To(Succeed())
		DeferCleanup(func() { _ = envTest.Delete(ctx, dep) })

		got := &appsv1.Deployment{}
		Expect(scopedCache.Get(ctx, client.ObjectKeyFromObject(dep), got)).To(Succeed())
	})

	It("rejects a Deployment read from an out-of-scope namespace", func() {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "test-deploy-oos", Namespace: "out-of-scope-ns"},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
				},
			},
		}
		Expect(envTest.Create(ctx, dep)).To(Succeed())
		DeferCleanup(func() { _ = envTest.Delete(ctx, dep) })

		got := &appsv1.Deployment{}
		err := scopedCache.Get(ctx, client.ObjectKeyFromObject(dep), got)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("unknown namespace"))
	})

	It("reads a Secret only from the infra namespace", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "test-secret", Namespace: "infra-ns"},
			StringData: map[string]string{"key": "val"},
		}
		Expect(envTest.Create(ctx, secret)).To(Succeed())
		DeferCleanup(func() { _ = envTest.Delete(ctx, secret) })

		got := &corev1.Secret{}
		Expect(scopedCache.Get(ctx, client.ObjectKeyFromObject(secret), got)).To(Succeed())

		oosSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "test-secret-oos", Namespace: "out-of-scope-ns"},
			StringData: map[string]string{"key": "val"},
		}
		Expect(envTest.Create(ctx, oosSecret)).To(Succeed())
		DeferCleanup(func() { _ = envTest.Delete(ctx, oosSecret) })

		err := scopedCache.Get(ctx, client.ObjectKeyFromObject(oosSecret), &corev1.Secret{})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("unknown namespace"))
	})
})
