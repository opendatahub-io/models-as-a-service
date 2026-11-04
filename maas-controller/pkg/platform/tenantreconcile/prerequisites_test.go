package tenantreconcile

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCollectPrerequisiteReportUsageLogsOnly(t *testing.T) {
	const appNamespace = "maas"
	dsciGVK := schema.GroupVersionKind{Group: "dscinitialization.opendatahub.io", Version: "v1", Kind: "DSCInitialization"}
	monitoringGVK := schema.GroupVersionKind{Group: "services.platform.opendatahub.io", Version: "v1alpha1", Kind: "Monitoring"}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, gvk := range []schema.GroupVersionKind{dsciGVK, monitoringGVK} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvkListKind(gvk), &unstructured.UnstructuredList{})
	}

	dsci := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": dsciGVK.GroupVersion().String(),
		"kind":       dsciGVK.Kind,
		"metadata":   map[string]any{"name": "default-dsci"},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "LokiStackAvailable", "status": "False", "reason": "LokiNotRequired"},
			map[string]any{"type": dsciConditionUsageLogsCollectorAvailable, "status": "True"},
			map[string]any{"type": dsciConditionPersesAvailable, "status": "True"},
		}},
	}}
	monitoring := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": monitoringGVK.GroupVersion().String(),
		"kind":       monitoringGVK.Kind,
		"metadata":   map[string]any{"name": "default-monitoring"},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": monitoringConditionUsageLogsLokiStackAvailable, "status": "True"},
		}},
	}}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: MaaSDBSecretName, Namespace: appNamespace},
		Data:       map[string][]byte{MaaSDBSecretKey: []byte("postgresql://example")},
	}

	for _, tc := range []struct {
		name          string
		stackStatus   string
		usageLogging  bool
		wantSubstring string
	}{
		{name: "healthy usage logs only", stackStatus: "True"},
		{name: "unavailable usage logs stack", stackStatus: "False", wantSubstring: "Monitoring usage-logs LokiStack is not available"},
		{name: "controller managed collector ignores Monitoring", stackStatus: "False", usageLogging: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := monitoring.DeepCopy()
			conditions, _, err := unstructured.NestedSlice(item.Object, "status", "conditions")
			if err != nil {
				t.Fatal(err)
			}
			conditions[0].(map[string]any)["status"] = tc.stackStatus
			if err := unstructured.SetNestedSlice(item.Object, conditions, "status", "conditions"); err != nil {
				t.Fatal(err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dsci.DeepCopy(), item, secret.DeepCopy()).Build()
			report := CollectPrerequisiteReport(context.Background(), c, appNamespace, false, tc.usageLogging, true)
			if len(report.Blocking) != 0 {
				t.Fatalf("unexpected blocking prerequisites: %v", report.Blocking)
			}
			if tc.wantSubstring == "" && len(report.Warnings) != 0 {
				t.Fatalf("unexpected warnings: %v", report.Warnings)
			}
			if tc.wantSubstring != "" && !strings.Contains(strings.Join(report.Warnings, "; "), tc.wantSubstring) {
				t.Fatalf("warnings %v do not contain %q", report.Warnings, tc.wantSubstring)
			}
		})
	}

	for _, tc := range []struct {
		name          string
		conditions    []any
		wantSubstring string
	}{
		{name: "missing condition", conditions: []any{}, wantSubstring: "condition not found"},
		{name: "unknown condition", conditions: []any{map[string]any{"type": monitoringConditionUsageLogsLokiStackAvailable, "status": "Unknown", "reason": "Installing"}}, wantSubstring: "UsageLogsLokiStackAvailable=Unknown: Installing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := monitoring.DeepCopy()
			if err := unstructured.SetNestedSlice(item.Object, tc.conditions, "status", "conditions"); err != nil {
				t.Fatal(err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(item).Build()
			warning := monitoringUsageLogsWarning(context.Background(), c)
			if !strings.Contains(warning, tc.wantSubstring) {
				t.Fatalf("warning %q does not contain %q", warning, tc.wantSubstring)
			}
		})
	}

	t.Run("missing Monitoring resource", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		warning := monitoringUsageLogsWarning(context.Background(), c)
		if !strings.Contains(warning, "default-monitoring not found") {
			t.Fatalf("unexpected warning: %q", warning)
		}
	})
}
