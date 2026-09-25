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

package maas

import (
	"context"
	"regexp"
	"strings"
	"testing"

	kservev1alpha2 "github.com/kserve/kserve/pkg/apis/serving/v1alpha2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

var dnsLabelPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func testPool(namespace, name string) maasv1alpha1.InferencePoolReference {
	return maasv1alpha1.InferencePoolReference{
		Group:     "inference.networking.k8s.io",
		Kind:      "InferencePool",
		Name:      name,
		Namespace: namespace,
	}
}

func TestInferenceObjectiveName(t *testing.T) {
	sub := types.NamespacedName{Namespace: "tenant-a", Name: "gold"}
	pool := testPool("models", "llama-pool")

	name := inferenceObjectiveName("acme", sub, pool)
	if !strings.HasPrefix(name, "maas-acme-gold-llama-pool-") {
		t.Errorf("name %q does not contain the readable tenant, subscription, and pool parts", name)
	}
	if got := inferenceObjectiveName("acme", sub, pool); got != name {
		t.Errorf("name is not deterministic: %q != %q", got, name)
	}

	distinct := map[string]string{
		"same subscription name in another tenant namespace": inferenceObjectiveName("other", types.NamespacedName{Namespace: "tenant-b", Name: "gold"}, pool),
		"another pool in the same namespace":                 inferenceObjectiveName("acme", sub, testPool("models", "mistral-pool")),
		"same pool name in another namespace":                inferenceObjectiveName("acme", sub, testPool("models-2", "llama-pool")),
		"another subscription":                               inferenceObjectiveName("acme", types.NamespacedName{Namespace: "tenant-a", Name: "silver"}, pool),
	}
	for desc, other := range distinct {
		if other == name {
			t.Errorf("%s: name collides with %q", desc, name)
		}
	}
}

func TestInferenceObjectiveName_LongIdentitiesAreTruncated(t *testing.T) {
	long := strings.Repeat("a", 60)
	sub := types.NamespacedName{Namespace: "tenant-a", Name: long + "-sub"}

	name := inferenceObjectiveName(long, sub, testPool("models", long+"-pool"))
	other := inferenceObjectiveName(long, sub, testPool("models", long+"-pool2"))

	for _, n := range []string{name, other} {
		if len(n) > inferenceObjectiveNameMaxLength {
			t.Errorf("name %q is %d characters, want at most %d", n, len(n), inferenceObjectiveNameMaxLength)
		}
		if !dnsLabelPattern.MatchString(n) {
			t.Errorf("name %q is not a valid DNS label", n)
		}
	}
	if name == other {
		t.Errorf("pools sharing a truncated prefix produced the same name %q", name)
	}
}

func TestInferenceObjectiveName_SanitizesDottedNames(t *testing.T) {
	name := inferenceObjectiveName(tenantreconcile.DefaultAITenantName,
		types.NamespacedName{Namespace: "tenant-a", Name: "gold.v2"}, testPool("models", "Pool.One"))
	if !dnsLabelPattern.MatchString(name) {
		t.Errorf("name %q is not a valid DNS label", name)
	}
}

// newLLMISvcWithPool returns a ready LLMInferenceService whose status reports poolName as its
// InferencePool. poolNamespace may be empty to use the service namespace.
func newLLMISvcWithPool(name, ns, poolName, poolNamespace string) *kservev1alpha2.LLMInferenceService {
	svc := newLLMISvc(name, ns, corev1.ConditionTrue)
	ref := &gatewayapiv1.ObjectReference{
		Group: "inference.networking.k8s.io",
		Kind:  "InferencePool",
		Name:  gatewayapiv1.ObjectName(poolName),
	}
	if poolNamespace != "" {
		poolNS := gatewayapiv1.Namespace(poolNamespace)
		ref.Namespace = &poolNS
	}
	svc.Status.Router = &kservev1alpha2.RouterStatus{
		Scheduler: &kservev1alpha2.ObservedSchedulerStatus{InferencePool: ref},
	}
	return svc
}

func newFlowControlSubscription(name, ns string, priority *int32, modelNames ...string) *maasv1alpha1.MaaSSubscription {
	sub := &maasv1alpha1.MaaSSubscription{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: maasv1alpha1.MaaSSubscriptionSpec{
			Owner:             maasv1alpha1.OwnerSpec{Groups: []maasv1alpha1.GroupReference{{Name: "team-a"}}},
			InferencePriority: priority,
		},
	}
	for _, m := range modelNames {
		sub.Spec.ModelRefs = append(sub.Spec.ModelRefs, maasv1alpha1.ModelSubscriptionRef{
			Name: m, Namespace: ns, TokenRateLimits: []maasv1alpha1.TokenRateLimit{{Limit: 100, Window: "1m"}},
		})
	}
	return sub
}

func flowControlStatusFor(t *testing.T, statuses []maasv1alpha1.ModelFlowControlStatus, model string) maasv1alpha1.ModelFlowControlStatus {
	t.Helper()
	for _, s := range statuses {
		if s.Name == model {
			return s
		}
	}
	t.Fatalf("no flow-control status for model %q in %+v", model, statuses)
	return maasv1alpha1.ModelFlowControlStatus{}
}

func TestResolveFlowControlStatuses(t *testing.T) {
	const ns = "default"
	zero := int32(0)
	ten := int32(10)

	stoppedGroupSvc := newLLMISvcWithPool("grouped-svc", ns, "grouped-pool", "")
	stoppedGroupSvc.Status.Router.Group = &kservev1alpha2.GroupStatus{
		Name: "rollout",
		Members: []kservev1alpha2.GroupMemberStatus{
			{Name: "grouped-svc", Weight: 100},
			{Name: "grouped-svc-old", Weight: 0, Stopped: true},
		},
	}
	splitGroupSvc := newLLMISvcWithPool("split-svc", ns, "split-pool", "")
	splitGroupSvc.Status.Router.Group = &kservev1alpha2.GroupStatus{
		Name: "canary",
		Members: []kservev1alpha2.GroupMemberStatus{
			{Name: "split-svc", Weight: 90},
			{Name: "split-svc-canary", Weight: 10},
		},
	}

	objects := []client.Object{
		newMaaSModelRef("llama", ns, "LLMInferenceService", "llama-svc"),
		newLLMISvcWithPool("llama-svc", ns, "shared-pool", ""),
		newMaaSModelRef("llama-alias", ns, "LLMInferenceService", "llama-alias-svc"),
		newLLMISvcWithPool("llama-alias-svc", ns, "shared-pool", ""),
		newMaaSModelRef("remote-pool", ns, "LLMInferenceService", "remote-pool-svc"),
		newLLMISvcWithPool("remote-pool-svc", ns, "remote-pool", "pools"),
		newMaaSModelRef("external", ns, "ExternalModel", "external"),
		newMaaSModelRef("starting", ns, "LLMInferenceService", "starting-svc"),
		newLLMISvc("starting-svc", ns, corev1.ConditionFalse),
		newMaaSModelRef("no-scheduler", ns, "LLMInferenceService", "no-scheduler-svc"),
		newLLMISvc("no-scheduler-svc", ns, corev1.ConditionTrue),
		newMaaSModelRef("missing-svc", ns, "LLMInferenceService", "does-not-exist"),
		newMaaSModelRef("grouped", ns, "LLMInferenceService", "grouped-svc"),
		stoppedGroupSvc,
		newMaaSModelRef("split", ns, "LLMInferenceService", "split-svc"),
		splitGroupSvc,
	}
	allModels := []string{"llama", "llama-alias", "remote-pool", "external", "starting", "no-scheduler", "missing-svc", "missing-ref", "grouped", "split"}

	tests := []struct {
		name          string
		priority      *int32
		wantPoolState maasv1alpha1.FlowControlState
	}{
		{name: "unset priority publishes names without objectives", priority: nil, wantPoolState: maasv1alpha1.FlowControlStateNotRequired},
		{name: "explicit zero requires an objective", priority: &zero, wantPoolState: maasv1alpha1.FlowControlStatePending},
		{name: "positive priority requires an objective", priority: &ten, wantPoolState: maasv1alpha1.FlowControlStatePending},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sub := newFlowControlSubscription("gold", ns, tc.priority, allModels...)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objects, sub)...).Build()
			r := &MaaSSubscriptionReconciler{Client: c, Scheme: scheme}

			statuses := r.resolveFlowControlStatuses(context.Background(), sub)
			if len(statuses) != len(allModels) {
				t.Fatalf("got %d statuses, want %d: %+v", len(statuses), len(allModels), statuses)
			}

			subKey := types.NamespacedName{Namespace: ns, Name: "gold"}
			tenant := tenantreconcile.DefaultAITenantName

			// Models backed by a single observed pool get the pool and objective name.
			for model, pool := range map[string]maasv1alpha1.InferencePoolReference{
				"llama":       testPool(ns, "shared-pool"),
				"llama-alias": testPool(ns, "shared-pool"),
				"remote-pool": testPool("pools", "remote-pool"),
				"grouped":     testPool(ns, "grouped-pool"),
			} {
				s := flowControlStatusFor(t, statuses, model)
				if s.State != tc.wantPoolState {
					t.Errorf("%s: state = %s, want %s (message %q)", model, s.State, tc.wantPoolState, s.Message)
				}
				if s.InferencePool == nil || *s.InferencePool != pool {
					t.Errorf("%s: inferencePool = %+v, want %+v", model, s.InferencePool, pool)
				}
				if want := inferenceObjectiveName(tenant, subKey, pool); s.ObjectiveName != want {
					t.Errorf("%s: objectiveName = %q, want %q", model, s.ObjectiveName, want)
				}
			}

			// Models sharing a pool share one objective.
			if a, b := flowControlStatusFor(t, statuses, "llama"), flowControlStatusFor(t, statuses, "llama-alias"); a.ObjectiveName != b.ObjectiveName {
				t.Errorf("models sharing a pool got different objective names: %q and %q", a.ObjectiveName, b.ObjectiveName)
			}

			// Models without a single pool report why, and never carry an objective name.
			for model, want := range map[string]maasv1alpha1.FlowControlState{
				"external":     maasv1alpha1.FlowControlStateNotApplicable,
				"no-scheduler": maasv1alpha1.FlowControlStateNotApplicable,
				"starting":     maasv1alpha1.FlowControlStatePending,
				"missing-svc":  maasv1alpha1.FlowControlStatePending,
				"missing-ref":  maasv1alpha1.FlowControlStatePending,
				"split":        maasv1alpha1.FlowControlStateUnsupported,
			} {
				s := flowControlStatusFor(t, statuses, model)
				if s.State != want {
					t.Errorf("%s: state = %s, want %s (message %q)", model, s.State, want, s.Message)
				}
				if s.ObjectiveName != "" || s.InferencePool != nil {
					t.Errorf("%s: unexpected pool %+v / objective %q", model, s.InferencePool, s.ObjectiveName)
				}
				if s.Message == "" {
					t.Errorf("%s: state %s has no message", model, s.State)
				}
			}
		})
	}
}

func TestResolveFlowControlStatuses_NameStableAcrossPriorityChanges(t *testing.T) {
	const ns = "default"
	one, two := int32(1), int32(-5)
	var names []string
	for _, p := range []*int32{nil, &one, &two} {
		sub := newFlowControlSubscription("gold", ns, p, "llama")
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			newMaaSModelRef("llama", ns, "LLMInferenceService", "llama-svc"),
			newLLMISvcWithPool("llama-svc", ns, "pool", ""),
			sub,
		).Build()
		r := &MaaSSubscriptionReconciler{Client: c, Scheme: scheme}
		names = append(names, r.resolveFlowControlStatuses(context.Background(), sub)[0].ObjectiveName)
	}
	if names[0] == "" || names[0] != names[1] || names[1] != names[2] {
		t.Errorf("objective name changed across priority set/change: %v", names)
	}
}

func TestResolveFlowControlStatuses_UsesAITenantName(t *testing.T) {
	const ns = "tenant-ns"
	tenantConfig := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: ns,
			Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
				tenantreconcile.LabelTenantName:        "acme",
			},
		},
	}
	sub := newFlowControlSubscription("gold", ns, nil, "llama")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenantConfig,
		newMaaSModelRef("llama", ns, "LLMInferenceService", "llama-svc"),
		newLLMISvcWithPool("llama-svc", ns, "pool", ""),
		sub,
	).Build()
	r := &MaaSSubscriptionReconciler{Client: c, Scheme: scheme}

	got := r.resolveFlowControlStatuses(context.Background(), sub)[0]
	want := inferenceObjectiveName("acme", types.NamespacedName{Namespace: ns, Name: "gold"}, testPool(ns, "pool"))
	if got.ObjectiveName != want {
		t.Errorf("objectiveName = %q, want %q", got.ObjectiveName, want)
	}
}

func TestResolveFlowControlStatuses_TenantErrorFailsOnlyPoolModels(t *testing.T) {
	const ns = "tenant-ns"
	// AITenant-managed config without a tenant name label is malformed.
	tenantConfig := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: ns,
			Labels:    map[string]string{tenantreconcile.LabelManagedByAITenant: "true"},
		},
	}
	sub := newFlowControlSubscription("gold", ns, nil, "llama", "external")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenantConfig,
		newMaaSModelRef("llama", ns, "LLMInferenceService", "llama-svc"),
		newLLMISvcWithPool("llama-svc", ns, "pool", ""),
		newMaaSModelRef("external", ns, "ExternalModel", "external"),
		sub,
	).Build()
	r := &MaaSSubscriptionReconciler{Client: c, Scheme: scheme}

	statuses := r.resolveFlowControlStatuses(context.Background(), sub)
	if s := flowControlStatusFor(t, statuses, "llama"); s.State != maasv1alpha1.FlowControlStateFailed || s.ObjectiveName != "" {
		t.Errorf("llama: state = %s, objectiveName = %q; want Failed with no name", s.State, s.ObjectiveName)
	}
	if s := flowControlStatusFor(t, statuses, "external"); s.State != maasv1alpha1.FlowControlStateNotApplicable {
		t.Errorf("external: state = %s, want NotApplicable", s.State)
	}
}

// TestMaaSSubscriptionReconciler_PublishesFlowControlStatuses verifies Reconcile persists the
// per-model flow-control mappings in subscription status.
func TestMaaSSubscriptionReconciler_PublishesFlowControlStatuses(t *testing.T) {
	const (
		ns        = "default"
		modelName = "llm"
		subName   = "sub-a"
	)
	model := newMaaSModelRef(modelName, ns, "ExternalModel", modelName)
	route := newHTTPRoute("maas-"+modelName, ns)
	sub := newMaaSSubscription(subName, ns, "team-a", modelName, 100)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(model, route, sub).
		WithStatusSubresource(&maasv1alpha1.MaaSSubscription{}).
		WithIndex(&maasv1alpha1.MaaSSubscription{}, "spec.modelRef", subscriptionModelRefIndexer).
		Build()
	r := &MaaSSubscriptionReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: subName, Namespace: ns}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := &maasv1alpha1.MaaSSubscription{}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Status.FlowControlStatuses) != 1 {
		t.Fatalf("flowControlStatuses = %+v, want one entry", got.Status.FlowControlStatuses)
	}
	if s := got.Status.FlowControlStatuses[0]; s.Name != modelName || s.State != maasv1alpha1.FlowControlStateNotApplicable {
		t.Errorf("flowControlStatuses[0] = %+v, want %s NotApplicable", s, modelName)
	}
}

func TestMapLLMISvcToMaaSSubscriptions(t *testing.T) {
	const ns = "default"
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			newMaaSModelRef("llama", ns, "LLMInferenceService", "llama-svc"),
			newMaaSModelRef("llama-2", ns, "LLMInferenceService", "llama-svc"),
			newMaaSModelRef("other", ns, "LLMInferenceService", "other-svc"),
			newMaaSModelRef("same-name-external", ns, "ExternalModel", "llama-svc"),
			newFlowControlSubscription("gold", ns, nil, "llama", "llama-2"),
			newFlowControlSubscription("silver", ns, nil, "llama-2"),
			newFlowControlSubscription("bronze", ns, nil, "other", "same-name-external"),
		).
		WithIndex(&maasv1alpha1.MaaSSubscription{}, modelRefIndexKey, subscriptionModelRefIndexer).
		Build()
	r := &MaaSSubscriptionReconciler{Client: c, Scheme: scheme}

	reqs := r.mapLLMISvcToMaaSSubscriptions(context.Background(), newLLMISvc("llama-svc", ns))
	got := map[string]bool{}
	for _, req := range reqs {
		if got[req.Name] {
			t.Errorf("duplicate request for %s", req.Name)
		}
		got[req.Name] = true
	}
	if len(got) != 2 || !got["gold"] || !got["silver"] {
		t.Errorf("requests = %v, want gold and silver", reqs)
	}
}

func TestLLMISvcRouterStatusChangedPredicate(t *testing.T) {
	p := llmisvcRouterStatusChangedPredicate{}
	base := newLLMISvcWithPool("svc", "default", "pool", "")

	unchanged := base.DeepCopy()
	unchanged.Labels = map[string]string{"touched": "true"}
	if p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: unchanged}) {
		t.Error("update without router or readiness change should be filtered")
	}

	newPool := base.DeepCopy()
	newPool.Status.Router.Scheduler.InferencePool.Name = "pool-2"
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: newPool}) {
		t.Error("InferencePool change should trigger reconcile")
	}

	notReady := base.DeepCopy()
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: notReady}) {
		t.Error("Ready change should trigger reconcile")
	}

	toUnstructured := func(svc *kservev1alpha2.LLMInferenceService) *unstructured.Unstructured {
		obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(svc)
		if err != nil {
			t.Fatalf("convert to unstructured: %v", err)
		}
		return &unstructured.Unstructured{Object: obj}
	}
	if p.Update(event.UpdateEvent{ObjectOld: toUnstructured(base), ObjectNew: toUnstructured(unchanged)}) {
		t.Error("unstructured update without router change should be filtered")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: toUnstructured(base), ObjectNew: toUnstructured(newPool)}) {
		t.Error("unstructured InferencePool change should trigger reconcile")
	}
}
