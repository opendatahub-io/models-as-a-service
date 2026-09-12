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

// usageLogsEFWithAccessLog builds an EnvoyFilter shaped like the real manifest: a CLUSTER
// patch with no access_log, followed by the NETWORK_FILTER patch that carries it.
func usageLogsEFWithAccessLog(accessLog []any) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"spec": map[string]any{
				"configPatches": []any{
					map[string]any{
						"applyTo": "CLUSTER",
						"patch":   map[string]any{"value": map[string]any{"name": "otel_als_cluster"}},
					},
					map[string]any{
						"applyTo": "NETWORK_FILTER",
						"patch": map[string]any{
							"value": map[string]any{
								"typed_config": map[string]any{
									"access_log": accessLog,
								},
							},
						},
					},
				},
			},
		},
	}
}

func usageLogsServiceNamespaceOf(g *WithT, ef *unstructured.Unstructured, accessLogIndex int) (string, bool) {
	g.THelper()
	configPatches, _, err := unstructured.NestedSlice(ef.Object, "spec", "configPatches")
	g.Expect(err).NotTo(HaveOccurred())
	patch, ok := configPatches[1].(map[string]any)
	g.Expect(ok).To(BeTrue())
	accessLog, _, err := unstructured.NestedSlice(patch, "patch", "value", "typed_config", "access_log")
	g.Expect(err).NotTo(HaveOccurred())
	entry, ok := accessLog[accessLogIndex].(map[string]any)
	g.Expect(ok).To(BeTrue())
	values, found, err := unstructured.NestedSlice(entry, "typed_config", "resource_attributes", "values")
	g.Expect(err).NotTo(HaveOccurred())
	if !found {
		return "", false
	}
	for _, v := range values {
		attr, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if k, _, _ := unstructured.NestedString(attr, "key"); k != "service.namespace" {
			continue
		}
		s, _, _ := unstructured.NestedString(attr, "value", "string_value")
		return s, true
	}
	return "", false
}

func otelAccessLogEntry(resourceAttributes []any) map[string]any {
	entry := map[string]any{
		"name":         "envoy.access_loggers.open_telemetry",
		"typed_config": map[string]any{"@type": "type.googleapis.com/OpenTelemetryAccessLogConfig"},
	}
	if resourceAttributes != nil {
		typedConfig, _ := entry["typed_config"].(map[string]any)
		typedConfig["resource_attributes"] = map[string]any{"values": resourceAttributes}
	}
	return entry
}

func TestPatchUsageLogsServiceNamespace(t *testing.T) {
	t.Run("overwrites the existing service.namespace value", func(t *testing.T) {
		g := NewWithT(t)

		ef := usageLogsEFWithAccessLog([]any{
			otelAccessLogEntry([]any{
				map[string]any{"key": "service.name", "value": map[string]any{"string_value": "models-as-a-service"}},
				map[string]any{"key": "service.namespace", "value": map[string]any{"string_value": "openshift-ingress"}},
			}),
		})

		g.Expect(PatchUsageLogsServiceNamespace(ef, "ai-tenant-redteam")).To(Succeed())

		ns, found := usageLogsServiceNamespaceOf(g, ef, 0)
		g.Expect(found).To(BeTrue())
		g.Expect(ns).To(Equal("ai-tenant-redteam"))

		// service.name must be left alone.
		configPatches, _, _ := unstructured.NestedSlice(ef.Object, "spec", "configPatches")
		patch, _ := configPatches[1].(map[string]any)
		accessLog, _, _ := unstructured.NestedSlice(patch, "patch", "value", "typed_config", "access_log")
		entry, _ := accessLog[0].(map[string]any)
		values, _, _ := unstructured.NestedSlice(entry, "typed_config", "resource_attributes", "values")
		g.Expect(values).To(HaveLen(2))
		first, _ := values[0].(map[string]any)
		name, _, _ := unstructured.NestedString(first, "value", "string_value")
		g.Expect(name).To(Equal("models-as-a-service"))
	})

	t.Run("appends when the attribute is absent", func(t *testing.T) {
		g := NewWithT(t)

		ef := usageLogsEFWithAccessLog([]any{otelAccessLogEntry(nil)})

		g.Expect(PatchUsageLogsServiceNamespace(ef, "ai-tenant-redteam")).To(Succeed())

		ns, found := usageLogsServiceNamespaceOf(g, ef, 0)
		g.Expect(found).To(BeTrue())
		g.Expect(ns).To(Equal("ai-tenant-redteam"))
	})

	t.Run("leaves non-OTel access loggers untouched", func(t *testing.T) {
		g := NewWithT(t)

		ef := usageLogsEFWithAccessLog([]any{
			map[string]any{
				"name":         "envoy.access_loggers.file",
				"typed_config": map[string]any{"@type": "type.googleapis.com/FileAccessLog"},
			},
			otelAccessLogEntry([]any{
				map[string]any{"key": "service.namespace", "value": map[string]any{"string_value": "openshift-ingress"}},
			}),
		})

		g.Expect(PatchUsageLogsServiceNamespace(ef, "ai-tenant-redteam")).To(Succeed())

		ns, found := usageLogsServiceNamespaceOf(g, ef, 1)
		g.Expect(found).To(BeTrue())
		g.Expect(ns).To(Equal("ai-tenant-redteam"))

		// The file logger must not gain a resource_attributes field.
		_, found = usageLogsServiceNamespaceOf(g, ef, 0)
		g.Expect(found).To(BeFalse())
	})

	t.Run("rejects an empty namespace", func(t *testing.T) {
		g := NewWithT(t)

		ef := usageLogsEFWithAccessLog([]any{otelAccessLogEntry(nil)})

		err := PatchUsageLogsServiceNamespace(ef, "")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("must not be empty"))
	})

	t.Run("errors when no OTel access logger is present", func(t *testing.T) {
		g := NewWithT(t)

		ef := usageLogsEFWithAccessLog([]any{
			map[string]any{"name": "envoy.access_loggers.file"},
		})

		err := PatchUsageLogsServiceNamespace(ef, "ai-tenant-redteam")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).NotTo(ContainSubstring("%!w"))
		g.Expect(err.Error()).To(ContainSubstring("no envoy.access_loggers.open_telemetry access_log entry"))
	})

	t.Run("errors when configPatches is missing", func(t *testing.T) {
		g := NewWithT(t)

		ef := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}

		err := PatchUsageLogsServiceNamespace(ef, "ai-tenant-redteam")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("configPatches not found or empty"))
	})

	t.Run("errors when an access_log entry is not an object", func(t *testing.T) {
		g := NewWithT(t)

		ef := usageLogsEFWithAccessLog([]any{"not-an-object"})

		err := PatchUsageLogsServiceNamespace(ef, "ai-tenant-redteam")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("access_log[0] is not an object"))
	})
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
