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

package v1alpha1_test

import (
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	pkgtest "github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestMaaSSubscriptionOrganizationRoundTrip(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "MaaSSubscription Organization Suite")
}

var envTest *pkgtest.Client

var _ = BeforeSuite(func(ctx SpecContext) {
	envTest = pkgtest.Configure(
		pkgtest.WithScheme(clientgoscheme.AddToScheme, maasv1alpha1.AddToScheme),
		pkgtest.WithCRDs(filepath.Join(
			pkgtest.ProjectRoot(),
			"deployment", "base", "maas-controller", "crd", "bases",
			"maas.opendatahub.io_maassubscriptions.yaml",
		)),
	).Start()
	Expect(envTest.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "maas-subscription-organization"}})).To(Succeed())
})

var _ = Describe("MaaSSubscription organization association", func() {
	const namespace = "maas-subscription-organization"

	It("round-trips associations and supports multiple subscriptions per organization", func(ctx SpecContext) {
		strict := client.FieldValidation(metav1.FieldValidationStrict)
		marked := organizationSubscription("marked", "grid-org-a")
		shared := organizationSubscription("shared", "grid-org-a")
		unmarked := organizationSubscription("legacy", "")

		Expect(envTest.Create(ctx, marked, strict)).To(Succeed())
		Expect(envTest.Create(ctx, shared, strict)).To(Succeed())
		Expect(envTest.Create(ctx, unmarked, strict)).To(Succeed())

		for _, name := range []string{"marked", "shared"} {
			var got maasv1alpha1.MaaSSubscription
			Expect(envTest.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &got)).To(Succeed())
			Expect(got.Spec.OrganizationID()).To(Equal("grid-org-a"))
		}

		var legacy maasv1alpha1.MaaSSubscription
		Expect(envTest.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "legacy"}, &legacy)).To(Succeed())
		Expect(legacy.Spec.OrganizationID()).To(BeEmpty())
	})

	It("safely adopts a legacy subscription once and then preserves the association", func(ctx SpecContext) {
		legacy := organizationSubscription("adopt-me", "")
		Expect(envTest.Create(ctx, legacy)).To(Succeed())

		legacy.Spec.Organization = &maasv1alpha1.SubscriptionOrganization{ID: "grid-org-a"}
		Expect(envTest.Update(ctx, legacy)).To(Succeed())

		var adopted maasv1alpha1.MaaSSubscription
		Expect(envTest.Get(ctx, client.ObjectKeyFromObject(legacy), &adopted)).To(Succeed())
		Expect(adopted.Spec.OrganizationID()).To(Equal("grid-org-a"))

		adopted.Spec.Organization = &maasv1alpha1.SubscriptionOrganization{ID: "grid-org-b"}
		err := envTest.Update(ctx, &adopted)
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("organization.id is immutable"))

		var current maasv1alpha1.MaaSSubscription
		Expect(envTest.Get(ctx, client.ObjectKeyFromObject(legacy), &current)).To(Succeed())
		current.Spec.Organization = nil
		err = envTest.Update(ctx, &current)
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("organization.id is immutable"))
	})

	It("rejects malformed organization identifiers", func(ctx SpecContext) {
		strict := client.FieldValidation(metav1.FieldValidationStrict)

		empty := organizationSubscription("empty-organization", "")
		empty.Spec.Organization = &maasv1alpha1.SubscriptionOrganization{ID: ""}
		err := envTest.Create(ctx, empty, strict)
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("spec.organization.id"))

		tooLong := organizationSubscription("long-organization", "")
		tooLong.Spec.Organization = &maasv1alpha1.SubscriptionOrganization{ID: strings.Repeat("a", 254)}
		err = envTest.Create(ctx, tooLong, strict)
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("spec.organization.id"))
	})

	It("keeps the association available for Pending, Failed, and Degraded subscriptions", func(ctx SpecContext) {
		sub := organizationSubscription("lifecycle", "grid-org-a")
		Expect(envTest.Create(ctx, sub)).To(Succeed())

		for _, phase := range []maasv1alpha1.Phase{
			maasv1alpha1.PhasePending,
			maasv1alpha1.PhaseFailed,
			maasv1alpha1.PhaseDegraded,
		} {
			var current maasv1alpha1.MaaSSubscription
			Expect(envTest.Get(ctx, client.ObjectKeyFromObject(sub), &current)).To(Succeed())
			current.Status.Phase = phase
			Expect(envTest.Status().Update(ctx, &current)).To(Succeed())

			var readBack maasv1alpha1.MaaSSubscription
			Expect(envTest.Get(ctx, client.ObjectKeyFromObject(sub), &readBack)).To(Succeed())
			Expect(readBack.Status.Phase).To(Equal(phase))
			Expect(readBack.Spec.OrganizationID()).To(Equal("grid-org-a"))
		}
	})
})

func organizationSubscription(name, organizationID string) *maasv1alpha1.MaaSSubscription {
	sub := &maasv1alpha1.MaaSSubscription{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "maas-subscription-organization"},
		Spec: maasv1alpha1.MaaSSubscriptionSpec{
			Owner: maasv1alpha1.OwnerSpec{
				Groups: []maasv1alpha1.GroupReference{{Name: "grid-users"}},
			},
			ModelRefs: []maasv1alpha1.ModelSubscriptionRef{{
				Name:      "model",
				Namespace: "maas-subscription-organization",
				Unlimited: true,
			}},
		},
	}
	if organizationID != "" {
		sub.Spec.Organization = &maasv1alpha1.SubscriptionOrganization{ID: organizationID}
	}
	return sub
}
