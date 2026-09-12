package tenantreconcile

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	. "github.com/onsi/gomega"
)

func TestPatchUsageLogsClusterAddress(t *testing.T) {
	g := NewWithT(t)

	ef := &unstructured.Unstructured{
		Object: map[string]any{
			"spec": map[string]any{
				"configPatches": []any{
					map[string]any{
						"patch": map[string]any{
							"value": map[string]any{
								"load_assignment": map[string]any{
									"endpoints": []any{},
								},
							},
						},
					},
				},
			},
		},
	}
	err := PatchUsageLogsClusterAddress(ef, "usage-logs-collector.opendatahub.svc")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).NotTo(ContainSubstring("%!w"))
	g.Expect(err.Error()).To(ContainSubstring("endpoints not found or empty"))
}

func TestPatchUsageLogsEnvoyFilterWorkloadSelector(t *testing.T) {
	g := NewWithT(t)

	ef := &unstructured.Unstructured{
		Object: map[string]any{
			"spec": map[string]any{
				"targetRefs": []any{
					map[string]any{"name": "original-gateway"},
				},
			},
		},
	}

	g.Expect(PatchUsageLogsEnvoyFilterWorkloadSelector(ef, "redteam-gateway")).To(Succeed())

	wsLabels, found, _ := unstructured.NestedStringMap(ef.Object, "spec", "workloadSelector", "labels")
	g.Expect(found).To(BeTrue())
	g.Expect(wsLabels["gateway.networking.k8s.io/gateway-name"]).To(Equal("redteam-gateway"))

	_, targetRefsFound, _ := unstructured.NestedSlice(ef.Object, "spec", "targetRefs")
	g.Expect(targetRefsFound).To(BeFalse())
}
