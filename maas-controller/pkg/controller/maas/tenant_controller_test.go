package maas

import (
	"testing"
	"time"

	netwv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"

	. "github.com/onsi/gomega"
)

type predicateUpdateCase[T client.Object] struct {
	name   string
	mutate func(T)
	want   bool
}

// expectPredicate runs update cases against copies of base and checks that
// create and delete events always pass. The new copy gets a fresh
// resourceVersion so each case models a real write rather than a resync.
func expectPredicate[T client.Object](t *testing.T, p predicate.Predicate, base T, cases []predicateUpdateCase[T]) {
	t.Helper()

	g := NewWithT(t)
	g.Expect(p.Create(event.CreateEvent{Object: base})).To(BeTrue(), "create")
	g.Expect(p.Delete(event.DeleteEvent{Object: base})).To(BeTrue(), "delete")

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			oldObj, ok := base.DeepCopyObject().(T)
			g.Expect(ok).To(BeTrue())
			newObj, ok := base.DeepCopyObject().(T)
			g.Expect(ok).To(BeTrue())
			newObj.SetResourceVersion(base.GetResourceVersion() + "1")
			tc.mutate(newObj)

			g.Expect(p.Update(event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj})).To(Equal(tc.want))
		})
	}
}

func TestTenantConfigChangedForTenant(t *testing.T) {
	base := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:            maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace:       "models-as-a-service",
			Generation:      1,
			ResourceVersion: "7",
			Labels:          map[string]string{tenantreconcile.LabelTenantName: "default"},
			Annotations:     map[string]string{managementStateAnnotation: managementStateManaged},
		},
	}

	expectPredicate(t, tenantConfigChangedForTenant(), base, []predicateUpdateCase[*maasv1alpha1.MaasTenantConfig]{
		{
			name: "status-only write is dropped",
			mutate: func(mtc *maasv1alpha1.MaasTenantConfig) {
				mtc.Status.Phase = "Active"
				mtc.Status.Conditions = []metav1.Condition{{Type: tenantreconcile.ReadyConditionType, Status: metav1.ConditionTrue}}
			},
			want: false,
		},
		{
			name:   "finalizer-only change is dropped",
			mutate: func(mtc *maasv1alpha1.MaasTenantConfig) { mtc.SetFinalizers([]string{tenantFinalizer}) },
			want:   false,
		},
		{
			name:   "informer resync passes",
			mutate: func(mtc *maasv1alpha1.MaasTenantConfig) { mtc.SetResourceVersion(base.ResourceVersion) },
			want:   true,
		},
		{
			name:   "spec change passes",
			mutate: func(mtc *maasv1alpha1.MaasTenantConfig) { mtc.SetGeneration(2) },
			want:   true,
		},
		{
			name: "management-state annotation change passes",
			mutate: func(mtc *maasv1alpha1.MaasTenantConfig) {
				mtc.SetAnnotations(map[string]string{managementStateAnnotation: managementStateRemoved})
			},
			want: true,
		},
		{
			name: "payload-processing handshake annotation passes",
			mutate: func(mtc *maasv1alpha1.MaasTenantConfig) {
				mtc.SetAnnotations(map[string]string{
					managementStateAnnotation:                         managementStateManaged,
					tenantreconcile.AnnotationPayloadProcessingStatus: tenantreconcile.PayloadProcessingStatusCleanupComplete,
				})
			},
			want: true,
		},
		{
			name: "label change passes",
			mutate: func(mtc *maasv1alpha1.MaasTenantConfig) {
				mtc.SetLabels(map[string]string{tenantreconcile.LabelTenantName: "team-a"})
			},
			want: true,
		},
		{
			name:   "deletion passes",
			mutate: func(mtc *maasv1alpha1.MaasTenantConfig) { mtc.SetDeletionTimestamp(&metav1.Time{Time: time.Now()}) },
			want:   true,
		},
	})
}

func TestAITenantPlatformContextChanged(t *testing.T) {
	base := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "ai-tenants", Generation: 1},
		Status: maasv1alpha1.AITenantStatus{
			Phase:      "Pending",
			GatewayRef: maasv1alpha1.TenantGatewayRef{Name: "team-a", Namespace: "openshift-ingress"},
		},
	}

	expectPredicate(t, aitenantPlatformContextChanged(), base, []predicateUpdateCase[*maasv1alpha1.AITenant]{
		{
			name: "phase and conditions change is dropped",
			mutate: func(at *maasv1alpha1.AITenant) {
				at.Status.Phase = "Active"
				at.Status.Conditions = []metav1.Condition{{Type: maasv1alpha1.AITenantConditionReady, Status: metav1.ConditionTrue}}
			},
			want: false,
		},
		{
			name: "status.gatewayRef change passes",
			mutate: func(at *maasv1alpha1.AITenant) {
				at.Status.GatewayRef.Name = "team-a-v2"
			},
			want: true,
		},
		{
			name: "spec.oidc change passes",
			mutate: func(at *maasv1alpha1.AITenant) {
				at.SetGeneration(2)
				at.Spec.OIDC = &maasv1alpha1.TenantExternalOIDCConfig{IssuerURL: "https://keycloak.example.com/realms/maas", ClientID: "maas"}
			},
			want: true,
		},
		{
			name: "spec.gateway change is dropped until it reaches status.gatewayRef",
			mutate: func(at *maasv1alpha1.AITenant) {
				at.SetGeneration(2)
				at.Spec.Gateway = &maasv1alpha1.AITenantGatewayRef{Name: "team-a-v2"}
			},
			want: false,
		},
		{
			name:   "deletion passes",
			mutate: func(at *maasv1alpha1.AITenant) { at.SetDeletionTimestamp(&metav1.Time{Time: time.Now()}) },
			want:   true,
		},
	})
}

func TestConfigSpecOrDeletionChanged(t *testing.T) {
	base := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, Generation: 1},
	}

	expectPredicate(t, configSpecOrDeletionChanged(), base, []predicateUpdateCase[*maasv1alpha1.Config]{
		{
			name: "status write is dropped",
			mutate: func(cfg *maasv1alpha1.Config) {
				cfg.Status.Conditions = []metav1.Condition{
					{Type: tenantreconcile.ReadyConditionType, Status: metav1.ConditionFalse, Reason: "OperandNotReady"},
				}
			},
			want: false,
		},
		{
			name:   "metadata-only change is dropped",
			mutate: func(cfg *maasv1alpha1.Config) { cfg.SetAnnotations(map[string]string{"example.com/touched": "true"}) },
			want:   false,
		},
		{
			name:   "spec change passes",
			mutate: func(cfg *maasv1alpha1.Config) { cfg.SetGeneration(2) },
			want:   true,
		},
		{
			name:   "deletion passes",
			mutate: func(cfg *maasv1alpha1.Config) { cfg.SetDeletionTimestamp(&metav1.Time{Time: time.Now()}) },
			want:   true,
		},
	})
}

func TestNetworkPolicyChangedForTenant(t *testing.T) {
	base := &netwv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "maas-api-cleanup",
			Namespace:  "opendatahub",
			Generation: 1,
			Labels: map[string]string{
				"app.kubernetes.io/part-of":          "models-as-a-service",
				tenantreconcile.LabelTenantName:      "tenant-a",
				tenantreconcile.LabelTenantNamespace: "ai-tenant-tenant-a",
			},
		},
	}

	expectPredicate(t, networkPolicyChangedForTenant(), base, []predicateUpdateCase[*netwv1.NetworkPolicy]{
		{
			// Real shared policies never carry tracking labels (the shared marker
			// replaces them post-render), so this relabel cannot happen through the
			// pipeline. Relabel suppression is gone either way: any label change is drift.
			name: "restamping the tracking labels is drift now that relabel suppression is gone",
			mutate: func(np *netwv1.NetworkPolicy) {
				np.Labels[tenantreconcile.LabelTenantName] = "tenant-b"
				np.Labels[tenantreconcile.LabelTenantNamespace] = "ai-tenant-tenant-b"
			},
			want: true,
		},
		{
			name:   "spec change passes",
			mutate: func(np *netwv1.NetworkPolicy) { np.SetGeneration(2) },
			want:   true,
		},
		{
			name: "managed=false annotation toggle passes",
			mutate: func(np *netwv1.NetworkPolicy) {
				np.SetAnnotations(map[string]string{tenantreconcile.AnnotationManaged: "false"})
			},
			want: true,
		},
		{
			name:   "non-tracking label change passes",
			mutate: func(np *netwv1.NetworkPolicy) { np.Labels["app.kubernetes.io/part-of"] = "other" },
			want:   true,
		},
		{
			name:   "deletion passes",
			mutate: func(np *netwv1.NetworkPolicy) { np.SetDeletionTimestamp(&metav1.Time{Time: time.Now()}) },
			want:   true,
		},
	})

	relabel := func(tenantName string) func(*netwv1.NetworkPolicy) {
		return func(np *netwv1.NetworkPolicy) {
			np.Labels[tenantreconcile.LabelTenantName] = tenantName
			np.Labels[tenantreconcile.LabelTenantNamespace] = "ai-tenant-" + tenantName
		}
	}
	perTenant := base.DeepCopy()
	perTenant.Name = tenantreconcile.PayloadProcessingNetworkPolicyName("tenant-a")
	perTenant.Namespace = "openshift-ingress"
	expectPredicate(t, networkPolicyChangedForTenant(), perTenant, []predicateUpdateCase[*netwv1.NetworkPolicy]{
		{name: "relabelling a per-tenant policy to another tenant passes", mutate: relabel("tenant-b"), want: true},
	})

	// The owner restoring its own labels is drift too: recovery comes from the mapped
	// handler invoking the mapper on both the old and the new object on Update
	// (controller-runtime), not from the predicate admitting only one direction.
	relabelled := perTenant.DeepCopy()
	relabel("tenant-b")(relabelled)
	expectPredicate(t, networkPolicyChangedForTenant(), relabelled, []predicateUpdateCase[*netwv1.NetworkPolicy]{
		{name: "the owner restoring its labels on a per-tenant policy passes", mutate: relabel("tenant-a"), want: true},
	})
}

func testDSCI(t *testing.T, conditions ...map[string]any) *unstructured.Unstructured {
	t.Helper()

	u := &unstructured.Unstructured{}
	u.SetAPIVersion("dscinitialization.opendatahub.io/v1")
	u.SetKind("DSCInitialization")
	u.SetName("default-dsci")
	u.SetGeneration(1)
	if len(conditions) > 0 {
		slice := make([]any, len(conditions))
		for i, c := range conditions {
			slice[i] = c
		}
		NewWithT(t).Expect(unstructured.SetNestedSlice(u.Object, slice, "status", "conditions")).To(Succeed())
	}
	return u
}

func TestDSCIMonitoringChanged(t *testing.T) {
	cond := func(typ, status, message string) map[string]any {
		return map[string]any{"type": typ, "status": status, "reason": "Reconciled", "message": message}
	}
	base := testDSCI(t,
		cond("MonitoringReady", "False", "waiting for monitoring stack"),
		cond("MonitoringStackAvailable", "True", ""),
		cond("PersesAvailable", "False", "perses not deployed"),
		cond("ReconcileComplete", "True", ""),
	)
	setConditions := func(conditions ...map[string]any) func(*unstructured.Unstructured) {
		return func(u *unstructured.Unstructured) {
			u.Object = testDSCI(t, conditions...).Object
		}
	}

	cases := []predicateUpdateCase[*unstructured.Unstructured]{
		{
			name: "untracked condition change is dropped",
			mutate: setConditions(
				cond("MonitoringReady", "False", "waiting for monitoring stack"),
				cond("MonitoringStackAvailable", "True", ""),
				cond("PersesAvailable", "False", "perses not deployed"),
				cond("ReconcileComplete", "False", "requeued"),
			),
			want: false,
		},
		{
			name: "spec or metadata change is dropped",
			mutate: func(u *unstructured.Unstructured) {
				u.SetGeneration(2)
				u.SetResourceVersion("42")
			},
			want: false,
		},
		{
			name: "change masked by an earlier failing condition is dropped",
			mutate: setConditions(
				cond("MonitoringReady", "False", "waiting for monitoring stack"),
				cond("MonitoringStackAvailable", "True", ""),
				cond("PersesAvailable", "False", "perses still installing"),
				cond("ReconcileComplete", "True", ""),
			),
			want: false,
		},
		{
			name: "tracked status change passes",
			mutate: setConditions(
				cond("MonitoringReady", "True", ""),
				cond("MonitoringStackAvailable", "True", ""),
				cond("PersesAvailable", "False", "perses not deployed"),
				cond("ReconcileComplete", "True", ""),
			),
			want: true,
		},
		{
			name: "tracked message change passes",
			mutate: setConditions(
				cond("MonitoringReady", "False", "monitoring namespace missing"),
				cond("MonitoringStackAvailable", "True", ""),
				cond("PersesAvailable", "False", "perses not deployed"),
				cond("ReconcileComplete", "True", ""),
			),
			want: true,
		},
		{
			name: "tracked condition disappearing passes",
			mutate: setConditions(
				cond("MonitoringStackAvailable", "True", ""),
				cond("PersesAvailable", "False", "perses not deployed"),
			),
			want: true,
		},
		{
			name:   "conditions cleared passes",
			mutate: setConditions(),
			want:   true,
		},
	}

	p := dsciMonitoringChanged()
	g := NewWithT(t)
	g.Expect(p.Create(event.TypedCreateEvent[*unstructured.Unstructured]{Object: base})).To(BeTrue())
	g.Expect(p.Delete(event.TypedDeleteEvent[*unstructured.Unstructured]{Object: base})).To(BeTrue())
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newObj := base.DeepCopy()
			tc.mutate(newObj)
			NewWithT(t).Expect(p.Update(event.TypedUpdateEvent[*unstructured.Unstructured]{ObjectOld: base.DeepCopy(), ObjectNew: newObj})).
				To(Equal(tc.want))
		})
	}

	t.Run("tracked condition appearing passes", func(t *testing.T) {
		oldObj := testDSCI(t, cond("ReconcileComplete", "True", ""))
		newObj := testDSCI(t, cond("ReconcileComplete", "True", ""), cond("MonitoringReady", "False", "starting"))
		NewWithT(t).Expect(p.Update(event.TypedUpdateEvent[*unstructured.Unstructured]{ObjectOld: oldObj, ObjectNew: newObj})).
			To(BeTrue())
	})
}

func TestTenantReconcileRateLimiterCapsBackoff(t *testing.T) {
	g := NewWithT(t)
	rl := tenantReconcileRateLimiter()
	item := reconcile.Request{NamespacedName: types.NamespacedName{
		Name:      maasv1alpha1.MaasTenantConfigInstanceName,
		Namespace: "models-as-a-service",
	}}

	g.Expect(rl.When(item)).To(Equal(tenantReconcileBaseBackoff))
	var last time.Duration
	for range 30 {
		last = rl.When(item)
	}
	g.Expect(last).To(Equal(tenantReconcileMaxBackoff))

	rl.Forget(item)
	g.Expect(rl.When(item)).To(Equal(tenantReconcileBaseBackoff))
}
