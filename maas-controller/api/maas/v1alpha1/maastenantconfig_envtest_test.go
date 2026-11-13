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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Keep aligned with AGC MaxExtProcMaxBodyBytes (pkg/tenant/constants.go).
const agcMaxExtProcMaxBodyBytes int64 = 33554432

var _ = Describe("MaasTenantConfig payloadProcessing.maxBodyBytes", func() {
	strict := client.FieldValidation(metav1.FieldValidationStrict)

	It("defaults maxBodyBytes to 10 MiB when payloadProcessing is empty", func(ctx SpecContext) {
		ns := uniqueNamespace(ctx, "mtc-default")
		cfg := maasTenantConfig(ns)
		cfg.Spec.PayloadProcessing = &maasv1alpha1.TenantPayloadProcessingConfig{}

		Expect(envTest.Create(ctx, cfg, strict)).To(Succeed())

		var got maasv1alpha1.MaasTenantConfig
		Expect(envTest.Get(ctx, client.ObjectKeyFromObject(cfg), &got)).To(Succeed())
		Expect(got.Spec.PayloadProcessing).NotTo(BeNil())
		Expect(got.Spec.PayloadProcessing.MaxBodyBytes).NotTo(BeNil())
		Expect(*got.Spec.PayloadProcessing.MaxBodyBytes).To(Equal(int64(10485760)))
	})

	It("accepts the AGC MaxExtProcMaxBodyBytes ceiling", func(ctx SpecContext) {
		ns := uniqueNamespace(ctx, "mtc-max")
		cfg := maasTenantConfig(ns)
		max := agcMaxExtProcMaxBodyBytes
		cfg.Spec.PayloadProcessing = &maasv1alpha1.TenantPayloadProcessingConfig{
			MaxBodyBytes: &max,
		}

		Expect(envTest.Create(ctx, cfg, strict)).To(Succeed())

		var got maasv1alpha1.MaasTenantConfig
		Expect(envTest.Get(ctx, client.ObjectKeyFromObject(cfg), &got)).To(Succeed())
		Expect(got.Spec.PayloadProcessing.MaxBodyBytes).NotTo(BeNil())
		Expect(*got.Spec.PayloadProcessing.MaxBodyBytes).To(Equal(agcMaxExtProcMaxBodyBytes))
	})

	It("rejects zero and values above the AGC ceiling", func(ctx SpecContext) {
		ns := uniqueNamespace(ctx, "mtc-reject")

		zero := int64(0)
		belowFloor := maasTenantConfig(ns)
		belowFloor.Spec.PayloadProcessing = &maasv1alpha1.TenantPayloadProcessingConfig{
			MaxBodyBytes: &zero,
		}
		err := envTest.Create(ctx, belowFloor, strict)
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("maxBodyBytes"))

		above := agcMaxExtProcMaxBodyBytes + 1
		aboveCeiling := maasTenantConfig(ns)
		aboveCeiling.Spec.PayloadProcessing = &maasv1alpha1.TenantPayloadProcessingConfig{
			MaxBodyBytes: &above,
		}
		err = envTest.Create(ctx, aboveCeiling, strict)
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("maxBodyBytes"))
	})
})

func uniqueNamespace(ctx SpecContext, prefix string) string {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix + "-"}}
	Expect(envTest.Create(ctx, ns)).To(Succeed())
	return ns.Name
}

func maasTenantConfig(namespace string) *maasv1alpha1.MaasTenantConfig {
	return &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: namespace,
		},
	}
}
