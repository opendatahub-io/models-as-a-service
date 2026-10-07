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
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	kservev1alpha2 "github.com/kserve/kserve/pkg/apis/serving/v1alpha2"
	llmdv1alpha2 "github.com/llm-d/llm-d-router/apix/v1alpha2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

// --- Fixtures ---

// acceptedTRLP returns the model's TokenRateLimitPolicy reporting Accepted, so a healthy
// subscription reaches Active with the fake client.
func acceptedTRLP(t *testing.T, model string) *unstructured.Unstructured {
	t.Helper()
	p := newPreexistingTRLP("maas-trlp-"+model, ioNS, model, nil)
	if err := unstructured.SetNestedSlice(p.Object, []any{
		map[string]any{"type": "Accepted", "status": "True"},
	}, "status", "conditions"); err != nil {
		t.Fatalf("SetNestedSlice status.conditions: %v", err)
	}
	return p
}

// healthyPoolModel returns poolModel with an accepted TokenRateLimitPolicy.
func healthyPoolModel(t *testing.T, model, pool string) []client.Object {
	t.Helper()
	return append(poolModel(model, pool), acceptedTRLP(t, model))
}

func storedSubscription(t *testing.T, c client.Client) *maasv1alpha1.MaaSSubscription {
	t.Helper()
	sub := &maasv1alpha1.MaaSSubscription{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ioNS, Name: ioSubName}, sub); err != nil {
		t.Fatalf("Get subscription: %v", err)
	}
	return sub
}

func findFlowControlStatus(t *testing.T, sub *maasv1alpha1.MaaSSubscription, model string) maasv1alpha1.ModelFlowControlStatus {
	t.Helper()
	for _, s := range sub.Status.FlowControlStatuses {
		if s.Name == model {
			return s
		}
	}
	t.Fatalf("no flow-control status for model %s in %+v", model, sub.Status.FlowControlStatuses)
	return maasv1alpha1.ModelFlowControlStatus{}
}

func onlyStatus(t *testing.T, statuses []maasv1alpha1.ModelFlowControlStatus) maasv1alpha1.ModelFlowControlStatus {
	t.Helper()
	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v, want exactly one", statuses)
	}
	return statuses[0]
}

func reconcileSubscription(t *testing.T, env *ioEnv) (ctrl.Result, error) {
	t.Helper()
	return env.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ioNS, Name: ioSubName}})
}

func withFinalizer(sub *maasv1alpha1.MaaSSubscription) *maasv1alpha1.MaaSSubscription {
	sub.Finalizers = []string{maasSubscriptionFinalizer}
	return sub
}

func foreignObjective(key types.NamespacedName) *llmdv1alpha2.InferenceObjective {
	return &llmdv1alpha2.InferenceObjective{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{"owner": "someone-else"}},
		Spec:       llmdv1alpha2.InferenceObjectiveSpec{Priority: ptrInt32(42), PoolRef: llmdv1alpha2.PoolObjectReference{Name: "llama-pool"}},
	}
}

func optedOutObjective(sub *maasv1alpha1.MaaSSubscription, key types.NamespacedName, priority int32) *llmdv1alpha2.InferenceObjective {
	obj := ownedObjective(sub, key, "llama-pool", priority)
	annotations := obj.GetAnnotations()
	annotations[ManagedByODHOperator] = "false"
	obj.SetAnnotations(annotations)
	return obj
}

// failObjectiveCreates returns interceptor funcs failing every InferenceObjective Create with err.
func failObjectiveCreates(err error) *interceptor.Funcs {
	return &interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*llmdv1alpha2.InferenceObjective); ok {
				return err
			}
			return cl.Create(ctx, obj, opts...)
		},
	}
}

// dropObjectiveCreates returns interceptor funcs that report InferenceObjective Creates as
// successful without storing them, as if the API server had not caught up yet.
func dropObjectiveCreates() *interceptor.Funcs {
	return &interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*llmdv1alpha2.InferenceObjective); ok {
				return nil
			}
			return cl.Create(ctx, obj, opts...)
		},
	}
}

// --- Per-model statuses ---

func TestReconcileInferenceObjectives_ModelStatuses(t *testing.T) {
	gr := schema.GroupResource{Group: llmdv1alpha2.GroupVersion.Group, Resource: "inferenceobjectives"}
	key := defaultObjectiveKey("llama-pool")
	prioritySub := newPrioritySubscription(ptrInt32(6), "llama")

	splitSvc := newLLMISvcWithPool("llama-svc", ioNS, "llama-pool", "")
	splitSvc.Status.Router.Group = &kservev1alpha2.GroupStatus{
		Name: "canary",
		Members: []kservev1alpha2.GroupMemberStatus{
			{Name: "llama-svc", Weight: 90},
			{Name: "llama-svc-canary", Weight: 10},
		},
	}
	offGateway := poolModel("llama", "llama-pool")
	route, ok := offGateway[2].(*gatewayapiv1.HTTPRoute)
	if !ok {
		t.Fatalf("offGateway[2] is %T", offGateway[2])
	}
	otherNS := gatewayapiv1.Namespace("other-gateway-ns")
	route.Spec.ParentRefs = []gatewayapiv1.ParentReference{{Name: "other-gateway", Namespace: &otherNS}}
	brokenTenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: ioNS,
			Labels:    map[string]string{tenantreconcile.LabelManagedByAITenant: "true"},
		},
	}

	tests := []struct {
		name        string
		priority    *int32
		objects     []client.Object
		funcs       *interceptor.Funcs
		unavailable bool

		wantReason    maasv1alpha1.FlowControlReason
		wantObjective bool // ObjectiveName is the llama-pool objective
		wantPool      bool // InferencePool is llama-pool
		wantErr       bool
		wantMessage   string
	}{
		{name: "reconciled", priority: ptrInt32(6), objects: poolModel("llama", "llama-pool"),
			wantReason: maasv1alpha1.FlowControlReasonObjectiveReconciled, wantObjective: true, wantPool: true},
		{name: "priority unset publishes the name", objects: poolModel("llama", "llama-pool"),
			wantReason: maasv1alpha1.FlowControlReasonPriorityUnset, wantObjective: true, wantPool: true},
		{name: "external model", priority: ptrInt32(6), objects: []client.Object{
			newMaaSModelRef("llama", ioNS, "ExternalModel", "llama"), newHTTPRoute("llama", ioNS)},
			wantReason: maasv1alpha1.FlowControlReasonNotApplicable},
		{name: "service starting", priority: ptrInt32(6), objects: []client.Object{
			newMaaSModelRef("llama", ioNS, "LLMInferenceService", "llama-svc"),
			newLLMISvc("llama-svc", ioNS, corev1.ConditionFalse), newLLMISvcRoute("llama-svc", ioNS)},
			wantReason: maasv1alpha1.FlowControlReasonPoolPending},
		{name: "route missing", priority: ptrInt32(6), objects: []client.Object{
			newMaaSModelRef("llama", ioNS, "LLMInferenceService", "llama-svc"), newLLMISvcWithPool("llama-svc", ioNS, "llama-pool", "")},
			wantReason: maasv1alpha1.FlowControlReasonPoolPending, wantPool: true},
		{name: "routing group", priority: ptrInt32(6), objects: []client.Object{
			newMaaSModelRef("llama", ioNS, "LLMInferenceService", "llama-svc"), splitSvc, newLLMISvcRoute("llama-svc", ioNS)},
			wantReason: maasv1alpha1.FlowControlReasonUnsupported},
		{name: "not on tenant gateway", priority: ptrInt32(6), objects: offGateway,
			wantReason: maasv1alpha1.FlowControlReasonNotOnTenantGateway, wantPool: true},
		{name: "foreign object holds the name", priority: ptrInt32(6), objects: append(poolModel("llama", "llama-pool"), foreignObjective(key)),
			wantReason: maasv1alpha1.FlowControlReasonObjectiveConflict, wantObjective: true, wantPool: true},
		{name: "opted out", priority: ptrInt32(6), objects: append(poolModel("llama", "llama-pool"), optedOutObjective(prioritySub, key, 2)),
			wantReason: maasv1alpha1.FlowControlReasonUnmanaged, wantObjective: true, wantPool: true, wantMessage: "(2)"},
		{name: "transient lookup error", priority: ptrInt32(6), objects: poolModel("llama", "llama-pool"), funcs: failLLMISvcGets("llama-svc"),
			wantReason: maasv1alpha1.FlowControlReasonReconcileFailed, wantErr: true},
		{name: "tenant lookup error", priority: ptrInt32(6), objects: append(poolModel("llama", "llama-pool"), brokenTenant),
			wantReason: maasv1alpha1.FlowControlReasonReconcileFailed, wantPool: true, wantErr: true},
		{name: "create failure", priority: ptrInt32(6), objects: poolModel("llama", "llama-pool"), funcs: failObjectiveCreates(errors.New("simulated")),
			wantReason: maasv1alpha1.FlowControlReasonReconcileFailed, wantObjective: true, wantPool: true, wantErr: true},
		{name: "stale cache", priority: ptrInt32(6), objects: poolModel("llama", "llama-pool"), funcs: failObjectiveCreates(apierrors.NewAlreadyExists(gr, key.Name)),
			wantReason: maasv1alpha1.FlowControlReasonObjectivePending, wantObjective: true, wantPool: true, wantErr: true},
		{name: "not observed after create", priority: ptrInt32(6), objects: poolModel("llama", "llama-pool"), funcs: dropObjectiveCreates(),
			wantReason: maasv1alpha1.FlowControlReasonObjectivePending, wantObjective: true, wantPool: true},
		{name: "API not installed", priority: ptrInt32(6), objects: poolModel("llama", "llama-pool"), unavailable: true,
			wantReason: maasv1alpha1.FlowControlReasonAPIUnavailable, wantObjective: true, wantPool: true},
		{name: "API not installed and priority unset", objects: poolModel("llama", "llama-pool"), unavailable: true,
			wantReason: maasv1alpha1.FlowControlReasonPriorityUnset, wantObjective: true, wantPool: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newIOEnv(t, tc.funcs, append(tc.objects, newPrioritySubscription(tc.priority, "llama"))...)
			env.r.inferenceObjectivesUnavailable.Store(tc.unavailable)

			res, err := env.reconcileIO(t)
			if (err != nil) != tc.wantErr {
				t.Fatalf("reconcile error = %v, want error %t", err, tc.wantErr)
			}
			got := onlyStatus(t, res.Statuses)
			if got.Name != "llama" || got.Namespace != ioNS {
				t.Errorf("status is for %s/%s, want %s/llama", got.Namespace, got.Name, ioNS)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("reason = %q (%s), want %q", got.Reason, got.Message, tc.wantReason)
			}
			if got.Ready != flowControlReasonReady(tc.wantReason) {
				t.Errorf("ready = %t for reason %q", got.Ready, got.Reason)
			}
			if got.Message == "" || !strings.Contains(got.Message, tc.wantMessage) {
				t.Errorf("message = %q, want it to contain %q", got.Message, tc.wantMessage)
			}
			wantName := ""
			if tc.wantObjective {
				wantName = key.Name
			}
			if got.ObjectiveName != wantName {
				t.Errorf("objectiveName = %q, want %q", got.ObjectiveName, wantName)
			}
			switch {
			case !tc.wantPool && got.InferencePool != nil:
				t.Errorf("inferencePool = %+v, want none", got.InferencePool)
			case tc.wantPool && (got.InferencePool == nil || got.InferencePool.Name != "llama-pool" ||
				got.InferencePool.Group != defaultInferencePoolGroup || got.InferencePool.Kind != defaultInferencePoolKind):
				t.Errorf("inferencePool = %+v, want llama-pool", got.InferencePool)
			}
		})
	}
}

func TestReconcileInferenceObjectives_SharedPoolStatuses(t *testing.T) {
	objs := []client.Object{
		newMaaSModelRef("chat", ioNS, "LLMInferenceService", "shared-svc"),
		newMaaSModelRef("code", ioNS, "LLMInferenceService", "shared-svc"),
		newLLMISvcWithPool("shared-svc", ioNS, "shared-pool", ""),
		newLLMISvcRoute("shared-svc", ioNS),
	}
	env := newIOEnv(t, nil, append(objs, newPrioritySubscription(ptrInt32(3), "code", "chat"))...)

	res, err := env.reconcileIO(t)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(res.Statuses) != 2 || res.Statuses[0].Name != "chat" || res.Statuses[1].Name != "code" {
		t.Fatalf("statuses = %+v, want chat then code", res.Statuses)
	}
	want := defaultObjectiveKey("shared-pool").Name
	for _, s := range res.Statuses {
		if s.ObjectiveName != want || s.Reason != maasv1alpha1.FlowControlReasonObjectiveReconciled {
			t.Errorf("%s: objectiveName = %q, reason = %q; want %q, %q", s.Name, s.ObjectiveName, s.Reason,
				want, maasv1alpha1.FlowControlReasonObjectiveReconciled)
		}
	}
	if objs := env.objectives(t); len(objs) != 1 {
		t.Errorf("objectives = %v, want one shared objective", objs)
	}
}

// --- Condition and phase ---

func TestReconcile_InferenceObjectivesConditionAndPhase(t *testing.T) {
	key := defaultObjectiveKey("llama-pool")
	prioritySub := newPrioritySubscription(ptrInt32(6), "llama")

	tests := []struct {
		name        string
		priority    *int32
		extra       []client.Object
		funcs       *interceptor.Funcs
		unavailable bool

		wantPhase     maasv1alpha1.Phase
		wantCondition metav1.ConditionStatus // "" means no condition
		wantReason    string
		wantErr       bool
	}{
		{name: "reconciled", priority: ptrInt32(6),
			wantPhase: maasv1alpha1.PhaseActive, wantCondition: metav1.ConditionTrue, wantReason: string(maasv1alpha1.ReasonReconciled)},
		{name: "priority unset",
			wantPhase: maasv1alpha1.PhaseActive},
		{name: "opted out", priority: ptrInt32(6), extra: []client.Object{optedOutObjective(prioritySub, key, 2)},
			wantPhase: maasv1alpha1.PhaseActive, wantCondition: metav1.ConditionTrue, wantReason: string(maasv1alpha1.ReasonReconciled)},
		{name: "conflict degrades", priority: ptrInt32(6), extra: []client.Object{foreignObjective(key)},
			wantPhase: maasv1alpha1.PhaseDegraded, wantCondition: metav1.ConditionFalse, wantReason: string(maasv1alpha1.FlowControlReasonObjectiveConflict)},
		{name: "create failure degrades", priority: ptrInt32(6), funcs: failObjectiveCreates(errors.New("simulated")),
			wantPhase: maasv1alpha1.PhaseDegraded, wantCondition: metav1.ConditionFalse, wantReason: string(maasv1alpha1.FlowControlReasonReconcileFailed), wantErr: true},
		{name: "pending does not degrade", priority: ptrInt32(6), funcs: dropObjectiveCreates(),
			wantPhase: maasv1alpha1.PhaseActive, wantCondition: metav1.ConditionFalse, wantReason: string(maasv1alpha1.FlowControlReasonObjectivePending)},
		{name: "API not installed does not degrade", priority: ptrInt32(6), unavailable: true,
			wantPhase: maasv1alpha1.PhaseActive, wantCondition: metav1.ConditionFalse, wantReason: string(maasv1alpha1.FlowControlReasonAPIUnavailable)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objs := append(healthyPoolModel(t, "llama", "llama-pool"), withFinalizer(newPrioritySubscription(tc.priority, "llama")))
			env := newIOEnv(t, tc.funcs, append(objs, tc.extra...)...)
			env.r.inferenceObjectivesUnavailable.Store(tc.unavailable)

			if _, err := reconcileSubscription(t, env); (err != nil) != tc.wantErr {
				t.Fatalf("Reconcile error = %v, want error %t", err, tc.wantErr)
			}
			sub := storedSubscription(t, env.c)
			ready := apimeta.FindStatusCondition(sub.Status.Conditions, "Ready")
			if sub.Status.Phase != tc.wantPhase {
				t.Errorf("phase = %q (%v), want %q", sub.Status.Phase, ready, tc.wantPhase)
			}
			if tc.wantPhase == maasv1alpha1.PhaseDegraded && (ready == nil || !strings.Contains(ready.Message, "request priority")) {
				t.Errorf("Ready condition %v does not explain the request-priority problem", ready)
			}
			cond := apimeta.FindStatusCondition(sub.Status.Conditions, maasv1alpha1.ConditionInferenceObjectivesReady)
			switch {
			case tc.wantCondition == "" && cond != nil:
				t.Errorf("condition = %+v, want none", cond)
			case tc.wantCondition != "" && cond == nil:
				t.Fatalf("condition %s missing", maasv1alpha1.ConditionInferenceObjectivesReady)
			case tc.wantCondition != "" && (cond.Status != tc.wantCondition || cond.Reason != tc.wantReason):
				t.Errorf("condition = %s/%s (%s), want %s/%s", cond.Status, cond.Reason, cond.Message, tc.wantCondition, tc.wantReason)
			}
			if len(sub.Status.FlowControlStatuses) != 1 {
				t.Errorf("flowControlStatuses = %+v, want one entry", sub.Status.FlowControlStatuses)
			}
		})
	}
}

// TestReconcile_ModelProblemsStillFail verifies request priority never raises the phase.
func TestReconcile_ModelProblemsStillFail(t *testing.T) {
	sub := withFinalizer(newPrioritySubscription(ptrInt32(6), "missing"))
	env := newIOEnv(t, nil, sub)
	if _, err := reconcileSubscription(t, env); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if phase := subscriptionPhase(t, env.c); phase != maasv1alpha1.PhaseFailed {
		t.Errorf("phase = %q, want %q for a subscription whose only model is missing", phase, maasv1alpha1.PhaseFailed)
	}
}

func TestReconcile_ClearingPriorityRemovesCondition(t *testing.T) {
	objs := append(healthyPoolModel(t, "llama", "llama-pool"), withFinalizer(newPrioritySubscription(ptrInt32(6), "llama")))
	env := newIOEnv(t, nil, objs...)
	if _, err := reconcileSubscription(t, env); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if apimeta.FindStatusCondition(storedSubscription(t, env.c).Status.Conditions, maasv1alpha1.ConditionInferenceObjectivesReady) == nil {
		t.Fatal("condition missing while inferencePriority is set")
	}

	env.updateSubscription(t, func(s *maasv1alpha1.MaaSSubscription) { s.Spec.InferencePriority = nil })
	if _, err := reconcileSubscription(t, env); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	sub := storedSubscription(t, env.c)
	if cond := apimeta.FindStatusCondition(sub.Status.Conditions, maasv1alpha1.ConditionInferenceObjectivesReady); cond != nil {
		t.Errorf("condition = %+v, want it removed once inferencePriority is cleared", cond)
	}
	if s := findFlowControlStatus(t, sub, "llama"); s.Reason != maasv1alpha1.FlowControlReasonPriorityUnset || s.ObjectiveName == "" {
		t.Errorf("status = %+v, want PriorityUnset with the objective name", s)
	}
	if objs := env.objectives(t); len(objs) != 0 {
		t.Errorf("objectives = %v, want none after clearing inferencePriority", objs)
	}
}

// TestReconcile_FlowControlStatusPersistedOnTRLPFailure verifies the status is written on
// the TokenRateLimitPolicy failure path too.
func TestReconcile_FlowControlStatusPersistedOnTRLPFailure(t *testing.T) {
	isTRLP := func(obj client.Object) bool {
		u, ok := obj.(*unstructured.Unstructured)
		return ok && u.GetKind() == "TokenRateLimitPolicy"
	}
	funcs := &interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if isTRLP(obj) {
				return errors.New("simulated TokenRateLimitPolicy failure")
			}
			return cl.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if isTRLP(obj) {
				return errors.New("simulated TokenRateLimitPolicy failure")
			}
			return cl.Update(ctx, obj, opts...)
		},
	}
	objs := append(poolModel("llama", "llama-pool"), withFinalizer(newPrioritySubscription(ptrInt32(6), "llama")))
	env := newIOEnv(t, funcs, objs...)
	if _, err := reconcileSubscription(t, env); err == nil {
		t.Fatal("Reconcile: expected the TokenRateLimitPolicy failure")
	}
	sub := storedSubscription(t, env.c)
	if sub.Status.Phase != maasv1alpha1.PhaseFailed {
		t.Errorf("phase = %q, want %q", sub.Status.Phase, maasv1alpha1.PhaseFailed)
	}
	if s := findFlowControlStatus(t, sub, "llama"); s.Reason != maasv1alpha1.FlowControlReasonObjectiveReconciled {
		t.Errorf("reason = %q, want %q", s.Reason, maasv1alpha1.FlowControlReasonObjectiveReconciled)
	}
	if apimeta.FindStatusCondition(sub.Status.Conditions, maasv1alpha1.ConditionInferenceObjectivesReady) == nil {
		t.Error("condition missing on the TokenRateLimitPolicy failure path")
	}
}

func TestInferenceObjectivesCondition(t *testing.T) {
	status := func(name string, reason maasv1alpha1.FlowControlReason) maasv1alpha1.ModelFlowControlStatus {
		return maasv1alpha1.ModelFlowControlStatus{Name: name, Namespace: ioNS, Reason: reason, Ready: flowControlReasonReady(reason)}
	}
	sub := newPrioritySubscription(ptrInt32(1))
	sub.Generation = 4

	tests := []struct {
		name         string
		statuses     []maasv1alpha1.ModelFlowControlStatus
		wantStatus   metav1.ConditionStatus
		wantReason   string
		wantDegraded bool
		wantMessage  string
	}{
		{name: "all ready", statuses: []maasv1alpha1.ModelFlowControlStatus{
			status("a", maasv1alpha1.FlowControlReasonObjectiveReconciled),
			status("b", maasv1alpha1.FlowControlReasonNotApplicable),
			status("c", maasv1alpha1.FlowControlReasonUnmanaged)},
			wantStatus: metav1.ConditionTrue, wantReason: string(maasv1alpha1.ReasonReconciled), wantMessage: "all 3 models"},
		{name: "most severe reason wins", statuses: []maasv1alpha1.ModelFlowControlStatus{
			status("a", maasv1alpha1.FlowControlReasonPoolPending),
			status("b", maasv1alpha1.FlowControlReasonObjectiveConflict),
			status("c", maasv1alpha1.FlowControlReasonUnsupported),
			status("d", maasv1alpha1.FlowControlReasonObjectiveReconciled)},
			wantStatus: metav1.ConditionFalse, wantReason: string(maasv1alpha1.FlowControlReasonObjectiveConflict), wantDegraded: true,
			wantMessage: "3 of 4 models: default/a (PoolPending), default/b (ObjectiveConflict), default/c (Unsupported)"},
		{name: "not on tenant gateway degrades", statuses: []maasv1alpha1.ModelFlowControlStatus{
			status("a", maasv1alpha1.FlowControlReasonNotOnTenantGateway)},
			wantStatus: metav1.ConditionFalse, wantReason: string(maasv1alpha1.FlowControlReasonNotOnTenantGateway), wantDegraded: true},
		{name: "pending only", statuses: []maasv1alpha1.ModelFlowControlStatus{
			status("a", maasv1alpha1.FlowControlReasonPoolPending),
			status("b", maasv1alpha1.FlowControlReasonObjectivePending)},
			wantStatus: metav1.ConditionFalse, wantReason: string(maasv1alpha1.FlowControlReasonObjectivePending)},
		{name: "API not installed", statuses: []maasv1alpha1.ModelFlowControlStatus{
			status("a", maasv1alpha1.FlowControlReasonAPIUnavailable)},
			wantStatus: metav1.ConditionFalse, wantReason: string(maasv1alpha1.FlowControlReasonAPIUnavailable)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cond, degraded, ok := inferenceObjectivesCondition(sub, tc.statuses)
			if !ok {
				t.Fatal("no condition while inferencePriority is set")
			}
			if cond.Type != maasv1alpha1.ConditionInferenceObjectivesReady || cond.ObservedGeneration != 4 {
				t.Errorf("condition type/generation = %s/%d", cond.Type, cond.ObservedGeneration)
			}
			if cond.Status != tc.wantStatus || cond.Reason != tc.wantReason || degraded != tc.wantDegraded {
				t.Errorf("condition = %s/%s degraded=%t, want %s/%s degraded=%t", cond.Status, cond.Reason, degraded,
					tc.wantStatus, tc.wantReason, tc.wantDegraded)
			}
			if !strings.Contains(cond.Message, tc.wantMessage) {
				t.Errorf("message = %q, want it to contain %q", cond.Message, tc.wantMessage)
			}
		})
	}

	if _, _, ok := inferenceObjectivesCondition(newPrioritySubscription(nil), nil); ok {
		t.Error("condition reported while inferencePriority is unset")
	}
}

// TestModelFlowControlSetReason_TruncatesMessage verifies messages fit the schema limit.
func TestModelFlowControlSetReason_TruncatesMessage(t *testing.T) {
	m := &modelFlowControl{}
	m.setReason(maasv1alpha1.FlowControlReasonReconcileFailed, strings.Repeat("é", flowControlMessageMaxLen))
	if n := len(m.Status.Message); n > flowControlMessageMaxLen {
		t.Errorf("message is %d bytes, want at most %d", n, flowControlMessageMaxLen)
	}
	if !utf8.ValidString(m.Status.Message) || !strings.HasSuffix(m.Status.Message, "…") {
		t.Errorf("message %q is not a valid truncated string", m.Status.Message)
	}

	m.setReason(maasv1alpha1.FlowControlReasonPoolPending, "short")
	if m.Status.Message != "short" {
		t.Errorf("short message changed to %q", m.Status.Message)
	}
}

// TestReconcileInferenceObjectives_ClearedPriorityCleansUpDespiteLookupError verifies a
// cleared priority deletes the objective even when a lookup fails.
func TestReconcileInferenceObjectives_ClearedPriorityCleansUpDespiteLookupError(t *testing.T) {
	sub := newPrioritySubscription(nil, "llama")
	broken := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: ioNS,
			Labels:    map[string]string{tenantreconcile.LabelManagedByAITenant: "true"},
		},
	}
	env := newIOEnv(t, nil, append(poolModel("llama", "llama-pool"), sub, broken, ownedObjective(sub, defaultObjectiveKey("llama-pool"), "llama-pool", 1))...)

	res, err := env.reconcileIO(t)
	if err == nil {
		t.Error("expected the tenant lookup error to be returned so the objective name is retried")
	}
	if objs := env.objectives(t); len(objs) != 0 {
		t.Errorf("objectives = %v, want the stale objective deleted", objs)
	}
	if s := onlyStatus(t, res.Statuses); s.Reason != maasv1alpha1.FlowControlReasonReconcileFailed {
		t.Errorf("reason = %q, want %q", s.Reason, maasv1alpha1.FlowControlReasonReconcileFailed)
	}
	if events := env.events(); len(events) != 0 {
		t.Errorf("unexpected events %v while inferencePriority is unset", events)
	}
}
