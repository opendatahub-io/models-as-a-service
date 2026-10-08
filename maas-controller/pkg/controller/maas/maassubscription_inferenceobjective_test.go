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
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	kservev1alpha2 "github.com/kserve/kserve/pkg/apis/serving/v1alpha2"
	llmdv1alpha2 "github.com/llm-d/llm-d-router/apix/v1alpha2"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

const (
	ioNS      = "default"
	ioSubName = "gold"
	ioSubUID  = types.UID("sub-uid-1")
)

// assertValidObjectiveName fails if name is not a valid InferenceObjective name.
func assertValidObjectiveName(t *testing.T, name string) {
	t.Helper()
	if len(name) > inferenceObjectiveNameMaxLength {
		t.Errorf("name %q is %d characters, want at most %d", name, len(name), inferenceObjectiveNameMaxLength)
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		t.Errorf("name %q is not a valid object name: %v", name, errs)
	}
}

// --- Fixtures ---

func ptrInt32(v int32) *int32 { return &v }

// newLLMISvcWithPool returns a ready LLMInferenceService whose status reports poolName as its
// InferencePool. poolNamespace may be empty to use the service namespace.
func newLLMISvcWithPool(name, ns, poolName, poolNamespace string) *kservev1alpha2.LLMInferenceService {
	svc := newLLMISvc(name, ns, corev1.ConditionTrue)
	ref := &gatewayapiv1.ObjectReference{
		Group: defaultInferencePoolGroup,
		Kind:  defaultInferencePoolKind,
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

// poolModel returns a MaaSModelRef, its ready LLMInferenceService reporting pool, and the
// service's HTTPRoute on the test gateway.
func poolModel(model, pool string) []client.Object {
	svc := model + "-svc"
	return []client.Object{
		newMaaSModelRef(model, ioNS, "LLMInferenceService", svc),
		newLLMISvcWithPool(svc, ioNS, pool, ""),
		newLLMISvcRoute(svc, ioNS),
	}
}

func newPrioritySubscription(priority *int32, models ...string) *maasv1alpha1.MaaSSubscription {
	sub := &maasv1alpha1.MaaSSubscription{
		ObjectMeta: metav1.ObjectMeta{Name: ioSubName, Namespace: ioNS, UID: ioSubUID},
		Spec: maasv1alpha1.MaaSSubscriptionSpec{
			Owner:             maasv1alpha1.OwnerSpec{Groups: []maasv1alpha1.GroupReference{{Name: "team-a"}}},
			InferencePriority: priority,
		},
	}
	for _, m := range models {
		sub.Spec.ModelRefs = append(sub.Spec.ModelRefs, maasv1alpha1.ModelSubscriptionRef{
			Name: m, Namespace: ioNS, TokenRateLimits: []maasv1alpha1.TokenRateLimit{{Limit: 100, Window: "1m"}},
		})
	}
	return sub
}

// objectiveKey is the key of the default-tenant objective for the test subscription and pool.
func objectiveKey(tenant, pool string) types.NamespacedName {
	return types.NamespacedName{
		Namespace: ioNS,
		Name:      inferenceObjectiveName(tenant, types.NamespacedName{Namespace: ioNS, Name: ioSubName}, types.NamespacedName{Namespace: ioNS, Name: pool}),
	}
}

func defaultObjectiveKey(pool string) types.NamespacedName {
	return objectiveKey(tenantreconcile.DefaultAITenantName, pool)
}

// ownedObjective returns an InferenceObjective as this controller would have written it.
func ownedObjective(sub *maasv1alpha1.MaaSSubscription, key types.NamespacedName, pool string, priority int32) *llmdv1alpha2.InferenceObjective {
	return buildInferenceObjective(sub, key, &desiredObjective{
		Pool:       inferencePoolRef{Group: defaultInferencePoolGroup, Kind: defaultInferencePoolKind, Name: pool},
		TenantName: tenantreconcile.DefaultAITenantName,
		Priority:   priority,
	})
}

type ioEnv struct {
	c   client.WithWatch
	r   *MaaSSubscriptionReconciler
	rec *events.FakeRecorder
}

func newIOEnv(t *testing.T, funcs *interceptor.Funcs, objs ...client.Object) *ioEnv {
	t.Helper()
	b := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(objs...).
		WithStatusSubresource(&maasv1alpha1.MaaSSubscription{}).
		WithIndex(&maasv1alpha1.MaaSSubscription{}, modelRefIndexKey, subscriptionModelRefIndexer)
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	c := b.Build()
	rec := events.NewFakeRecorder(100)
	return &ioEnv{
		c:   c,
		rec: rec,
		r: &MaaSSubscriptionReconciler{
			Client:                 c,
			APIReader:              c,
			Scheme:                 scheme,
			AppNamespace:           "odh-ai-gateway-infra",
			DefaultTenantNamespace: ioNS,
			GatewayName:            testGatewayName,
			GatewayNamespace:       testGatewayNamespace,
			Recorder:               rec,
		},
	}
}

// reconcileIO runs the InferenceObjective pass for the stored subscription.
func (e *ioEnv) reconcileIO(t *testing.T) (inferenceObjectivesResult, error) {
	t.Helper()
	sub := &maasv1alpha1.MaaSSubscription{}
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: ioNS, Name: ioSubName}, sub); err != nil {
		t.Fatalf("Get subscription: %v", err)
	}
	return e.r.reconcileInferenceObjectives(context.Background(), logr.Discard(), sub)
}

func (e *ioEnv) updateSubscription(t *testing.T, mutate func(*maasv1alpha1.MaaSSubscription)) {
	t.Helper()
	sub := &maasv1alpha1.MaaSSubscription{}
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: ioNS, Name: ioSubName}, sub); err != nil {
		t.Fatalf("Get subscription: %v", err)
	}
	mutate(sub)
	if err := e.c.Update(context.Background(), sub); err != nil {
		t.Fatalf("Update subscription: %v", err)
	}
}

func (e *ioEnv) objectives(t *testing.T) map[types.NamespacedName]*llmdv1alpha2.InferenceObjective {
	t.Helper()
	list := &llmdv1alpha2.InferenceObjectiveList{}
	if err := e.c.List(context.Background(), list); err != nil {
		t.Fatalf("List InferenceObjectives: %v", err)
	}
	out := make(map[types.NamespacedName]*llmdv1alpha2.InferenceObjective, len(list.Items))
	for i := range list.Items {
		out[client.ObjectKeyFromObject(&list.Items[i])] = &list.Items[i]
	}
	return out
}

// events drains the recorded events.
func (e *ioEnv) events() []string {
	var out []string
	for {
		select {
		case ev := <-e.rec.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func hasEvent(events []string, reason string) bool {
	for _, ev := range events {
		if strings.HasPrefix(ev, corev1.EventTypeWarning+" "+reason+" ") {
			return true
		}
	}
	return false
}

func objectivePriority(t *testing.T, obj *llmdv1alpha2.InferenceObjective) int32 {
	t.Helper()
	if obj == nil || obj.Spec.Priority == nil {
		t.Fatalf("spec.priority missing on %v", obj)
	}
	return *obj.Spec.Priority
}

func objectivePoolRef(t *testing.T, obj *llmdv1alpha2.InferenceObjective) map[string]string {
	t.Helper()
	if obj == nil {
		t.Fatal("objective missing")
		return nil
	}
	ref := obj.Spec.PoolRef
	return map[string]string{"group": string(ref.Group), "kind": string(ref.Kind), "name": string(ref.Name)}
}

// countInferenceObjectiveWrites returns interceptor funcs counting InferenceObjective Updates.
func countInferenceObjectiveUpdates(updates *atomic.Int32) *interceptor.Funcs {
	return &interceptor.Funcs{
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*llmdv1alpha2.InferenceObjective); ok {
				updates.Add(1)
			}
			return cl.Update(ctx, obj, opts...)
		},
	}
}

// failLLMISvcGets returns interceptor funcs that fail Get for the named LLMInferenceService.
func failLLMISvcGets(name string) *interceptor.Funcs {
	return &interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*kservev1alpha2.LLMInferenceService); ok && key.Name == name {
				return errors.New("simulated API server error")
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}
}

// --- Name ---

func TestInferenceObjectiveName(t *testing.T) {
	sub := types.NamespacedName{Namespace: "tenant-a", Name: "gold"}
	pool := types.NamespacedName{Namespace: "models", Name: "llama-pool"}

	name := inferenceObjectiveName("acme", sub, pool)
	if !strings.HasPrefix(name, "maas-acme-gold-llama-pool-") {
		t.Errorf("name %q does not contain the readable tenant, subscription, and pool parts", name)
	}
	if got := inferenceObjectiveName("acme", sub, pool); got != name {
		t.Errorf("name is not deterministic: %q != %q", got, name)
	}

	distinct := map[string]string{
		"same subscription name in another tenant namespace": inferenceObjectiveName("other", types.NamespacedName{Namespace: "tenant-b", Name: "gold"}, pool),
		"another pool in the same namespace":                 inferenceObjectiveName("acme", sub, types.NamespacedName{Namespace: "models", Name: "mistral-pool"}),
		"same pool name in another namespace":                inferenceObjectiveName("acme", sub, types.NamespacedName{Namespace: "models-2", Name: "llama-pool"}),
		"another subscription":                               inferenceObjectiveName("acme", types.NamespacedName{Namespace: "tenant-a", Name: "silver"}, pool),
		"another tenant":                                     inferenceObjectiveName("acme-2", sub, pool),
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

	name := inferenceObjectiveName(long, sub, types.NamespacedName{Namespace: "models", Name: long + "-pool"})
	other := inferenceObjectiveName(long, sub, types.NamespacedName{Namespace: "models", Name: long + "-pool2"})

	for _, n := range []string{name, other} {
		assertValidObjectiveName(t, n)
	}
	if name == other {
		t.Errorf("pools sharing a truncated prefix produced the same name %q", name)
	}
}

// TestInferenceObjectiveName_DottedNames verifies names keep the dots of Kubernetes object
// names and stay valid wherever truncation cuts them.
func TestInferenceObjectiveName_DottedNames(t *testing.T) {
	sub := types.NamespacedName{Namespace: "tenant-a", Name: "gold.v2"}
	name := inferenceObjectiveName(tenantreconcile.DefaultAITenantName, sub, types.NamespacedName{Namespace: "models", Name: "pool.one"})
	if !strings.HasPrefix(name, "maas-models-as-a-service-gold.v2-pool.one-") {
		t.Errorf("name %q does not keep the dotted subscription and pool names", name)
	}
	assertValidObjectiveName(t, name)

	// Slide a dot through a long tenant name so truncation cuts right before, on, and after it.
	pool := types.NamespacedName{Namespace: "models", Name: "pool"}
	for i := 1; i <= inferenceObjectiveNameMaxLength; i++ {
		tenant := strings.Repeat("a", i) + ".b" + strings.Repeat("b", inferenceObjectiveNameMaxLength)
		assertValidObjectiveName(t, inferenceObjectiveName(tenant, sub, pool))
	}
}

// --- Create and update ---

func TestReconcileInferenceObjectives_Create(t *testing.T) {
	for _, priority := range []int32{0, 7, -3} {
		env := newIOEnv(t, nil, append(poolModel("llama", "llama-pool"), newPrioritySubscription(&priority, "llama"))...)
		if _, err := env.reconcileIO(t); err != nil {
			t.Fatalf("priority %d: reconcileInferenceObjectives: %v", priority, err)
		}

		objs := env.objectives(t)
		key := defaultObjectiveKey("llama-pool")
		obj, ok := objs[key]
		if len(objs) != 1 || !ok {
			t.Fatalf("priority %d: objectives = %v, want only %s", priority, objs, key)
		}
		if got := objectivePriority(t, obj); got != priority {
			t.Errorf("priority %d: spec.priority = %d", priority, got)
		}
		wantPool := map[string]string{"group": defaultInferencePoolGroup, "kind": defaultInferencePoolKind, "name": "llama-pool"}
		if got := objectivePoolRef(t, obj); !mapsEqual(got, wantPool) {
			t.Errorf("priority %d: spec.poolRef = %v, want %v", priority, got, wantPool)
		}
		labels := obj.GetLabels()
		if !hasInferenceObjectiveOwnerLabels(labels) || labels[labelSubscriptionNamespace] != ioNS || labels[labelSubscriptionUID] != string(ioSubUID) {
			t.Errorf("priority %d: labels = %v", priority, labels)
		}
		annotations := obj.GetAnnotations()
		if annotations[annotationSubscriptionName] != ioSubName || annotations[annotationInferenceObjTenantName] != tenantreconcile.DefaultAITenantName {
			t.Errorf("priority %d: annotations = %v", priority, annotations)
		}
		if events := env.events(); len(events) != 0 {
			t.Errorf("priority %d: unexpected events %v", priority, events)
		}
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestReconcileInferenceObjectives_UpdatesInPlace(t *testing.T) {
	var updates atomic.Int32
	env := newIOEnv(t, countInferenceObjectiveUpdates(&updates),
		append(poolModel("llama", "llama-pool"), newPrioritySubscription(ptrInt32(1), "llama"))...)
	if _, err := env.reconcileIO(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := env.reconcileIO(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := updates.Load(); got != 0 {
		t.Fatalf("unchanged reconcile issued %d Updates, want 0", got)
	}

	env.updateSubscription(t, func(s *maasv1alpha1.MaaSSubscription) { s.Spec.InferencePriority = ptrInt32(5) })
	if _, err := env.reconcileIO(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	objs := env.objectives(t)
	obj, ok := objs[defaultObjectiveKey("llama-pool")]
	if len(objs) != 1 || !ok {
		t.Fatalf("objectives = %v, want one with the same name", objs)
	}
	if got := objectivePriority(t, obj); got != 5 {
		t.Errorf("spec.priority = %d, want 5", got)
	}
	if got := updates.Load(); got != 1 {
		t.Errorf("priority change issued %d Updates, want 1", got)
	}
	if _, err := env.reconcileIO(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := updates.Load(); got != 1 {
		t.Errorf("unchanged reconcile after update issued Updates; total %d, want 1", got)
	}
}

// TestReconcileInferenceObjectives_RevertsSpecEdits verifies an edit to an owned objective's
// spec or ownership labels is reverted while unrelated metadata is kept.
func TestReconcileInferenceObjectives_RevertsSpecEdits(t *testing.T) {
	sub := newPrioritySubscription(ptrInt32(4), "llama")
	key := defaultObjectiveKey("llama-pool")
	edited := ownedObjective(sub, key, "llama-pool", 99)
	labels := edited.GetLabels()
	labels["team"] = "ops"
	edited.SetLabels(labels)
	env := newIOEnv(t, nil, append(poolModel("llama", "llama-pool"), sub, edited)...)

	if _, err := env.reconcileIO(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	obj := env.objectives(t)[key]
	if got := objectivePriority(t, obj); got != 4 {
		t.Errorf("spec.priority = %d, want 4", got)
	}
	if obj.GetLabels()["team"] != "ops" {
		t.Errorf("unrelated label was dropped: %v", obj.GetLabels())
	}
}

// --- Desired set ---

func TestReconcileInferenceObjectives_ClearingPriorityDeletes(t *testing.T) {
	env := newIOEnv(t, nil, append(poolModel("llama", "llama-pool"), newPrioritySubscription(ptrInt32(3), "llama"))...)
	if _, err := env.reconcileIO(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(env.objectives(t)) != 1 {
		t.Fatal("expected an objective after setting priority")
	}
	env.updateSubscription(t, func(s *maasv1alpha1.MaaSSubscription) { s.Spec.InferencePriority = nil })
	if _, err := env.reconcileIO(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if objs := env.objectives(t); len(objs) != 0 {
		t.Errorf("objectives = %v, want none after clearing priority", objs)
	}
}

func TestReconcileInferenceObjectives_OneObjectivePerPool(t *testing.T) {
	objs := append(poolModel("llama", "shared-pool"), poolModel("llama-alias", "shared-pool")...)
	objs = append(objs, poolModel("mistral", "mistral-pool")...)
	env := newIOEnv(t, nil, append(objs, newPrioritySubscription(ptrInt32(2), "llama", "llama-alias", "mistral", "llama"))...)
	if _, err := env.reconcileIO(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := env.objectives(t)
	if len(got) != 2 {
		t.Fatalf("objectives = %v, want one per pool", got)
	}
	for _, pool := range []string{"shared-pool", "mistral-pool"} {
		if _, ok := got[defaultObjectiveKey(pool)]; !ok {
			t.Errorf("no objective for pool %s", pool)
		}
	}
}

func TestReconcileInferenceObjectives_RemovedModelAndPoolChangeDeleteOld(t *testing.T) {
	objs := append(poolModel("llama", "llama-pool"), poolModel("mistral", "mistral-pool")...)
	env := newIOEnv(t, nil, append(objs, newPrioritySubscription(ptrInt32(2), "llama", "mistral"))...)
	if _, err := env.reconcileIO(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Remove mistral.
	env.updateSubscription(t, func(s *maasv1alpha1.MaaSSubscription) { s.Spec.ModelRefs = s.Spec.ModelRefs[:1] })
	if _, err := env.reconcileIO(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := env.objectives(t)
	if _, ok := got[defaultObjectiveKey("llama-pool")]; !ok || len(got) != 1 {
		t.Fatalf("after removing a model: objectives = %v, want only llama-pool", got)
	}

	// Move llama to another pool.
	svc := &kservev1alpha2.LLMInferenceService{}
	if err := env.c.Get(context.Background(), types.NamespacedName{Namespace: ioNS, Name: "llama-svc"}, svc); err != nil {
		t.Fatalf("Get LLMInferenceService: %v", err)
	}
	svc.Status.Router.Scheduler.InferencePool.Name = "llama-pool-v2"
	if err := env.c.Status().Update(context.Background(), svc); err != nil {
		if err := env.c.Update(context.Background(), svc); err != nil {
			t.Fatalf("Update LLMInferenceService: %v", err)
		}
	}
	if _, err := env.reconcileIO(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got = env.objectives(t)
	if _, ok := got[defaultObjectiveKey("llama-pool-v2")]; !ok || len(got) != 1 {
		t.Errorf("after pool change: objectives = %v, want only llama-pool-v2", got)
	}
}

// --- Pool details ---

func TestReconcileInferenceObjectives_PoolGroup(t *testing.T) {
	objs := poolModel("llama", "llama-pool")
	svc, ok := objs[1].(*kservev1alpha2.LLMInferenceService)
	if !ok {
		t.Fatalf("objs[1] is %T", objs[1])
	}
	svc.Status.Router.Scheduler.InferencePool.Group = "inference.networking.x-k8s.io"
	env := newIOEnv(t, nil, append(objs, newPrioritySubscription(ptrInt32(1), "llama"))...)
	if _, err := env.reconcileIO(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	key := defaultObjectiveKey("llama-pool")
	if got := objectivePoolRef(t, env.objectives(t)[key])["group"]; got != "inference.networking.x-k8s.io" {
		t.Fatalf("poolRef.group = %q, want the observed group", got)
	}

	// KServe flips the group when the gateway accepts the v1 pool: same name, new group.
	stored := &kservev1alpha2.LLMInferenceService{}
	if err := env.c.Get(context.Background(), client.ObjectKeyFromObject(svc), stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	stored.Status.Router.Scheduler.InferencePool.Group = ""
	stored.Status.Router.Scheduler.InferencePool.Kind = ""
	if err := env.c.Status().Update(context.Background(), stored); err != nil {
		if err := env.c.Update(context.Background(), stored); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
	if _, err := env.reconcileIO(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := env.objectives(t)
	if len(got) != 1 {
		t.Fatalf("objectives = %v, want the same single objective", got)
	}
	ref := objectivePoolRef(t, got[key])
	if ref["group"] != defaultInferencePoolGroup || ref["kind"] != defaultInferencePoolKind {
		t.Errorf("poolRef = %v, want defaulted group and kind", ref)
	}
}

func TestReconcileInferenceObjectives_ModelsWithoutSinglePool(t *testing.T) {
	splitSvc := newLLMISvcWithPool("split-svc", ioNS, "split-pool", "")
	splitSvc.Status.Router.Group = &kservev1alpha2.GroupStatus{
		Name: "canary",
		Members: []kservev1alpha2.GroupMemberStatus{
			{Name: "split-svc", Weight: 90},
			{Name: "split-svc-canary", Weight: 10},
		},
	}
	stoppedSvc := newLLMISvcWithPool("grouped-svc", ioNS, "grouped-pool", "")
	stoppedSvc.Status.Router.Group = &kservev1alpha2.GroupStatus{
		Name: "rollout",
		Members: []kservev1alpha2.GroupMemberStatus{
			{Name: "grouped-svc", Weight: 100},
			{Name: "grouped-svc-old", Weight: 0, Stopped: true},
		},
	}

	tests := []struct {
		name      string
		objects   []client.Object
		wantEvent bool
		wantPool  string
	}{
		{name: "split", wantEvent: true, objects: []client.Object{
			newMaaSModelRef("split", ioNS, "LLMInferenceService", "split-svc"), splitSvc, newLLMISvcRoute("split-svc", ioNS)}},
		{name: "remote-pool", wantEvent: true, objects: []client.Object{
			newMaaSModelRef("remote-pool", ioNS, "LLMInferenceService", "remote-svc"),
			newLLMISvcWithPool("remote-svc", ioNS, "remote-pool", "pools"), newLLMISvcRoute("remote-svc", ioNS)}},
		{name: "grouped", wantPool: "grouped-pool", objects: []client.Object{
			newMaaSModelRef("grouped", ioNS, "LLMInferenceService", "grouped-svc"), stoppedSvc, newLLMISvcRoute("grouped-svc", ioNS)}},
		{name: "external", objects: []client.Object{
			newMaaSModelRef("external", ioNS, "ExternalModel", "external"), newHTTPRoute("external", ioNS)}},
		{name: "no-scheduler", objects: []client.Object{
			newMaaSModelRef("no-scheduler", ioNS, "LLMInferenceService", "no-scheduler-svc"),
			newLLMISvc("no-scheduler-svc", ioNS, corev1.ConditionTrue), newLLMISvcRoute("no-scheduler-svc", ioNS)}},
		{name: "starting", objects: []client.Object{
			newMaaSModelRef("starting", ioNS, "LLMInferenceService", "starting-svc"),
			newLLMISvc("starting-svc", ioNS, corev1.ConditionFalse), newLLMISvcRoute("starting-svc", ioNS)}},
		{name: "missing-svc", objects: []client.Object{
			newMaaSModelRef("missing-svc", ioNS, "LLMInferenceService", "does-not-exist")}},
		{name: "missing-ref"},
		{name: "no-route", objects: []client.Object{
			newMaaSModelRef("no-route", ioNS, "LLMInferenceService", "no-route-svc"),
			newLLMISvcWithPool("no-route-svc", ioNS, "no-route-pool", "")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newIOEnv(t, nil, append(tc.objects, newPrioritySubscription(ptrInt32(1), tc.name))...)
			if _, err := env.reconcileIO(t); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			objs := env.objectives(t)
			if tc.wantPool != "" {
				if _, ok := objs[defaultObjectiveKey(tc.wantPool)]; !ok || len(objs) != 1 {
					t.Errorf("objectives = %v, want one for %s", objs, tc.wantPool)
				}
			} else if len(objs) != 0 {
				t.Errorf("objectives = %v, want none", objs)
			}
			if got := hasEvent(env.events(), eventReasonInferencePoolUnsupported); got != tc.wantEvent {
				t.Errorf("Unsupported warning = %t, want %t", got, tc.wantEvent)
			}
		})
	}
}

// --- Gateway ---

func TestReconcileInferenceObjectives_GatewayMismatch(t *testing.T) {
	sub := newPrioritySubscription(ptrInt32(1), "llama")
	key := defaultObjectiveKey("llama-pool")
	objs := poolModel("llama", "llama-pool")
	route, ok := objs[2].(*gatewayapiv1.HTTPRoute)
	if !ok {
		t.Fatalf("objs[2] is %T", objs[2])
	}
	otherNS := gatewayapiv1.Namespace("other-gateway-ns")
	route.Spec.ParentRefs = []gatewayapiv1.ParentReference{{Name: "other-gateway", Namespace: &otherNS}}
	env := newIOEnv(t, nil, append(objs, sub, ownedObjective(sub, key, "llama-pool", 1))...)

	if _, err := env.reconcileIO(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := env.objectives(t); len(got) != 0 {
		t.Errorf("objectives = %v, want the owned objective deleted", got)
	}
	if !hasEvent(env.events(), eventReasonInferencePoolNotOnGateway) {
		t.Error("expected a gateway mismatch warning")
	}
}

func TestValidateHTTPRouteReferencesGateway_WrapsSentinelOnlyForMismatch(t *testing.T) {
	gw := maasv1alpha1.TenantGatewayRef{Name: testGatewayName, Namespace: testGatewayNamespace}
	otherNS := gatewayapiv1.Namespace("elsewhere")
	mismatch := newHTTPRoute("mismatch", ioNS)
	mismatch.Spec.ParentRefs = []gatewayapiv1.ParentReference{{Name: "other", Namespace: &otherNS}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mismatch).Build()

	err := validateHTTPRouteReferencesGateway(context.Background(), c, "mismatch", ioNS, gw)
	if !errors.Is(err, ErrHTTPRouteNotOnTenantGateway) {
		t.Errorf("mismatch error %v does not wrap ErrHTTPRouteNotOnTenantGateway", err)
	}
	if want := "HTTPRoute default/mismatch does not reference tenant Gateway openshift-ingress/maas-default-gateway"; err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}

	err = validateHTTPRouteReferencesGateway(context.Background(), c, "missing", ioNS, gw)
	if err == nil || errors.Is(err, ErrHTTPRouteNotOnTenantGateway) {
		t.Errorf("missing route error %v must not wrap ErrHTTPRouteNotOnTenantGateway", err)
	}
}

// --- Conflicts, opt-out, and adoption ---

func TestReconcileInferenceObjectives_ForeignObjectIsNotTouched(t *testing.T) {
	key := defaultObjectiveKey("llama-pool")
	foreign := &llmdv1alpha2.InferenceObjective{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{"owner": "someone-else"}},
		Spec:       llmdv1alpha2.InferenceObjectiveSpec{Priority: ptrInt32(42), PoolRef: llmdv1alpha2.PoolObjectReference{Name: "llama-pool"}},
	}

	sub := newPrioritySubscription(ptrInt32(1), "llama")
	sub.Finalizers = []string{maasSubscriptionFinalizer}
	env := newIOEnv(t, nil, append(poolModel("llama", "llama-pool"), sub, foreign)...)

	res, err := env.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ioNS, Name: ioSubName}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != inferenceObjectiveConflictRequeue {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, inferenceObjectiveConflictRequeue)
	}
	got := env.objectives(t)[key]
	if objectivePriority(t, got) != 42 || got.GetLabels()["owner"] != "someone-else" || hasInferenceObjectiveOwnerLabels(got.GetLabels()) {
		t.Errorf("foreign objective was modified: %+v", got)
	}
	if !hasEvent(env.events(), eventReasonInferenceObjectiveConflict) {
		t.Error("expected a conflict warning")
	}
}

func TestReconcileInferenceObjectives_OtherSubscriptionsObjectIsNotTouched(t *testing.T) {
	key := defaultObjectiveKey("llama-pool")
	other := newPrioritySubscription(ptrInt32(9), "llama")
	other.Name = "silver"
	other.UID = "other-uid"
	theirs := ownedObjective(other, key, "llama-pool", 9)

	env := newIOEnv(t, nil, append(poolModel("llama", "llama-pool"), newPrioritySubscription(ptrInt32(1), "llama"), theirs)...)
	res, err := env.reconcileIO(t)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !res.Conflict {
		t.Error("expected conflict for an objective owned by another subscription")
	}
	got := env.objectives(t)[key]
	if objectivePriority(t, got) != 9 || got.GetAnnotations()[annotationSubscriptionName] != "silver" {
		t.Errorf("other subscription's objective was modified: %+v", got)
	}
}

func TestReconcileInferenceObjectives_AdoptsStaleUID(t *testing.T) {
	sub := newPrioritySubscription(ptrInt32(6), "llama")
	key := defaultObjectiveKey("llama-pool")
	previous := sub.DeepCopy()
	previous.UID = "deleted-subscription-uid"
	env := newIOEnv(t, nil, append(poolModel("llama", "llama-pool"), sub, ownedObjective(previous, key, "llama-pool", 2))...)

	if _, err := env.reconcileIO(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := env.objectives(t)[key]
	if got.GetLabels()[labelSubscriptionUID] != string(ioSubUID) {
		t.Errorf("UID label = %q, want adopted %q", got.GetLabels()[labelSubscriptionUID], ioSubUID)
	}
	if objectivePriority(t, got) != 6 {
		t.Errorf("spec.priority = %d, want 6", objectivePriority(t, got))
	}
}

func TestReconcileInferenceObjectives_OptedOutIsNeverUpdatedOrDeleted(t *testing.T) {
	sub := newPrioritySubscription(ptrInt32(6), "llama")
	key := defaultObjectiveKey("llama-pool")
	optedOut := ownedObjective(sub, key, "llama-pool", 2)
	annotations := optedOut.GetAnnotations()
	annotations[ManagedByODHOperator] = "false"
	optedOut.SetAnnotations(annotations)
	env := newIOEnv(t, nil, append(poolModel("llama", "llama-pool"), sub, optedOut)...)

	if _, err := env.reconcileIO(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := objectivePriority(t, env.objectives(t)[key]); got != 2 {
		t.Errorf("opted-out objective updated: priority %d", got)
	}

	env.updateSubscription(t, func(s *maasv1alpha1.MaaSSubscription) { s.Spec.InferencePriority = nil })
	if _, err := env.reconcileIO(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := env.objectives(t)[key]; !ok {
		t.Error("opted-out objective was deleted")
	}
}

// --- Tenant ---

// aiTenantConfig returns an AITenant-managed MaasTenantConfig for tenantName and its AITenant,
// whose gateway is the test gateway.
func aiTenantConfig(tenantName string) []client.Object {
	config := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: ioNS,
			Labels: map[string]string{
				tenantreconcile.LabelManagedByAITenant: "true",
				tenantreconcile.LabelTenantName:        tenantName,
			},
			Annotations: map[string]string{
				tenantreconcile.AnnotationAITenantName:      tenantName,
				tenantreconcile.AnnotationAITenantNamespace: "aitenants",
			},
		},
	}
	aitenant := &maasv1alpha1.AITenant{
		ObjectMeta: metav1.ObjectMeta{Name: tenantName, Namespace: "aitenants"},
		Status: maasv1alpha1.AITenantStatus{
			GatewayRef: maasv1alpha1.TenantGatewayRef{Name: testGatewayName, Namespace: testGatewayNamespace},
		},
	}
	return []client.Object{config, aitenant}
}

func TestReconcileInferenceObjectives_TenantRename(t *testing.T) {
	env := newIOEnv(t, nil, append(poolModel("llama", "llama-pool"), newPrioritySubscription(ptrInt32(1), "llama"))...)
	if _, err := env.reconcileIO(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := env.objectives(t)[defaultObjectiveKey("llama-pool")]; !ok {
		t.Fatal("expected the default-tenant objective")
	}

	// An AITenant adopts the namespace's tenant config.
	for _, obj := range aiTenantConfig("acme") {
		if err := env.c.Create(context.Background(), obj); err != nil {
			t.Fatalf("Create %T: %v", obj, err)
		}
	}
	if _, err := env.reconcileIO(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := env.objectives(t)
	obj, ok := got[objectiveKey("acme", "llama-pool")]
	if !ok || len(got) != 1 {
		t.Fatalf("objectives = %v, want only the acme objective", got)
	}
	if obj.GetAnnotations()[annotationInferenceObjTenantName] != "acme" {
		t.Errorf("tenant annotation = %q, want acme", obj.GetAnnotations()[annotationInferenceObjTenantName])
	}
}

func TestReconcileInferenceObjectives_TenantErrorKeepsObjectives(t *testing.T) {
	sub := newPrioritySubscription(ptrInt32(1), "llama")
	key := defaultObjectiveKey("llama-pool")
	// AITenant-managed config without a tenant name label is malformed.
	broken := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: ioNS,
			Labels:    map[string]string{tenantreconcile.LabelManagedByAITenant: "true"},
		},
	}
	env := newIOEnv(t, nil, append(poolModel("llama", "llama-pool"), sub, broken, ownedObjective(sub, key, "llama-pool", 1))...)

	if _, err := env.reconcileIO(t); err == nil {
		t.Fatal("expected the tenant lookup error to be returned for retry")
	}
	if _, ok := env.objectives(t)[key]; !ok {
		t.Error("existing objective was deleted after a tenant lookup error")
	}
	if !hasEvent(env.events(), eventReasonInferenceObjectiveFailed) {
		t.Error("expected a failure warning")
	}
}

func TestFlowControlTenantName(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(aiTenantConfig("acme")...).Build()
	if got, err := flowControlTenantName(context.Background(), c, ioNS); err != nil || got != "acme" {
		t.Errorf("tenant name = %q, %v; want acme", got, err)
	}
	if got, err := flowControlTenantName(context.Background(), c, "no-config"); err != nil || got != tenantreconcile.DefaultAITenantName {
		t.Errorf("tenant name without config = %q, %v; want %s", got, err, tenantreconcile.DefaultAITenantName)
	}
}

// --- Independence ---

func TestReconcile_TransientErrorOnOneModelKeepsItsObjective(t *testing.T) {
	sub := newPrioritySubscription(ptrInt32(1), "flaky", "llama")
	sub.Finalizers = []string{maasSubscriptionFinalizer}
	flakyKey := defaultObjectiveKey("flaky-pool")
	objs := append(poolModel("flaky", "flaky-pool"), poolModel("llama", "llama-pool")...)
	objs = append(objs, sub, ownedObjective(sub, flakyKey, "flaky-pool", 1))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ioNS, Name: ioSubName}}

	env := newIOEnv(t, failLLMISvcGets("flaky-svc"), objs...)
	if _, err := env.r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("Reconcile: expected the transient error to be returned for retry")
	}
	got := env.objectives(t)
	if _, ok := got[flakyKey]; !ok {
		t.Error("flaky model's objective was deleted after a transient error")
	}
	if _, ok := got[defaultObjectiveKey("llama-pool")]; !ok {
		t.Error("healthy model's objective was not created")
	}
	stored := storedSubscription(t, env.c)
	if s := findFlowControlStatus(t, stored, "flaky"); s.Reason != maasv1alpha1.FlowControlReasonReconcileFailed {
		t.Errorf("flaky reason = %q, want %q", s.Reason, maasv1alpha1.FlowControlReasonReconcileFailed)
	}
	if s := findFlowControlStatus(t, stored, "llama"); s.Reason != maasv1alpha1.FlowControlReasonObjectiveReconciled {
		t.Errorf("llama reason = %q, want %q", s.Reason, maasv1alpha1.FlowControlReasonObjectiveReconciled)
	}
}

func subscriptionPhase(t *testing.T, c client.Client) maasv1alpha1.Phase {
	t.Helper()
	sub := &maasv1alpha1.MaaSSubscription{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ioNS, Name: ioSubName}, sub); err != nil {
		t.Fatalf("Get subscription: %v", err)
	}
	return sub.Status.Phase
}

func TestReconcileInferenceObjectives_CreateFailureDoesNotBlockOtherPools(t *testing.T) {
	funcs := &interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if o, ok := obj.(*llmdv1alpha2.InferenceObjective); ok && o.Spec.PoolRef.Name == "a-pool" {
				return errors.New("simulated create failure")
			}
			return cl.Create(ctx, obj, opts...)
		},
	}
	objs := append(poolModel("a", "a-pool"), poolModel("b", "b-pool")...)
	env := newIOEnv(t, funcs, append(objs, newPrioritySubscription(ptrInt32(1), "a", "b"))...)

	if _, err := env.reconcileIO(t); err == nil {
		t.Fatal("expected the create failure to be returned")
	}
	if _, ok := env.objectives(t)[defaultObjectiveKey("b-pool")]; !ok {
		t.Error("objective for b-pool was not created")
	}
	if !hasEvent(env.events(), eventReasonInferenceObjectiveFailed) {
		t.Error("expected a failure warning")
	}
}

// TestReconcileInferenceObjectives_StaleCacheRetriesQuietly verifies AlreadyExists and
// Conflict, which a cache behind our own last write produces, are retried without a
// Warning event.
func TestReconcileInferenceObjectives_StaleCacheRetriesQuietly(t *testing.T) {
	gr := schema.GroupResource{Group: llmdv1alpha2.GroupVersion.Group, Resource: "inferenceobjectives"}
	for name, writeErr := range map[string]error{
		"AlreadyExists": apierrors.NewAlreadyExists(gr, "obj"),
		"Conflict":      apierrors.NewConflict(gr, "obj", errors.New("stale resourceVersion")),
	} {
		t.Run(name, func(t *testing.T) {
			sub := newPrioritySubscription(ptrInt32(2), "llama")
			objs := append(poolModel("llama", "llama-pool"), sub)
			if name == "Conflict" {
				objs = append(objs, ownedObjective(sub, defaultObjectiveKey("llama-pool"), "llama-pool", 1))
			}
			env := newIOEnv(t, &interceptor.Funcs{
				Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if _, ok := obj.(*llmdv1alpha2.InferenceObjective); ok {
						return writeErr
					}
					return cl.Create(ctx, obj, opts...)
				},
				Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if _, ok := obj.(*llmdv1alpha2.InferenceObjective); ok {
						return writeErr
					}
					return cl.Update(ctx, obj, opts...)
				},
			}, objs...)

			if _, err := env.reconcileIO(t); err == nil {
				t.Fatal("expected the error to be returned for retry")
			}
			if events := env.events(); len(events) != 0 {
				t.Errorf("unexpected events %v", events)
			}
		})
	}
}

// --- API missing ---

func TestReconcileInferenceObjectives_APIMissing(t *testing.T) {
	sub := newPrioritySubscription(ptrInt32(1), "llama")
	sub.Finalizers = []string{maasSubscriptionFinalizer}
	objs := append(poolModel("llama", "llama-pool"), sub)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ioNS, Name: ioSubName}}

	t.Run("GVK not served", func(t *testing.T) {
		var calls atomic.Int32
		noMatch := &apimeta.NoKindMatchError{
			GroupKind:        schema.GroupKind{Group: llmdv1alpha2.GroupVersion.Group, Kind: "InferenceObjective"},
			SearchedVersions: []string{llmdv1alpha2.GroupVersion.Version},
		}
		isObjective := func(obj runtime.Object) bool {
			switch obj.(type) {
			case *llmdv1alpha2.InferenceObjective, *llmdv1alpha2.InferenceObjectiveList:
				return true
			}
			return false
		}
		env := newIOEnv(t, &interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if isObjective(obj) {
					calls.Add(1)
					return noMatch
				}
				return cl.Get(ctx, key, obj, opts...)
			},
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if isObjective(obj) {
					return noMatch
				}
				return cl.Create(ctx, obj, opts...)
			},
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if isObjective(list) {
					return noMatch
				}
				return cl.List(ctx, list, opts...)
			},
		}, objs...)
		if _, err := env.r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if calls.Load() == 0 {
			t.Error("the InferenceObjective pass never reached the API")
		}
		if phase := subscriptionPhase(t, env.c); phase == "" {
			t.Error("status was not reconciled")
		}
		if err := env.r.deleteOwnedInferenceObjectives(context.Background(), logr.Discard(), sub); err != nil {
			t.Errorf("deleteOwnedInferenceObjectives: %v, want nil when the API is not served", err)
		}
	})

	t.Run("unavailable flag set", func(t *testing.T) {
		calls := atomic.Int32{}
		funcs := &interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*llmdv1alpha2.InferenceObjective); ok {
					calls.Add(1)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*llmdv1alpha2.InferenceObjectiveList); ok {
					calls.Add(1)
				}
				return cl.List(ctx, list, opts...)
			},
		}
		env := newIOEnv(t, funcs, objs...)
		env.r.inferenceObjectivesUnavailable.Store(true)
		if _, err := env.r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if calls.Load() != 0 {
			t.Errorf("InferenceObjective API called %d times with the unavailable flag set", calls.Load())
		}
		if phase := subscriptionPhase(t, env.c); phase == "" {
			t.Error("status was not reconciled")
		}
	})
}

// --- Deletion ---

func deleteSubscription(t *testing.T, env *ioEnv) error {
	t.Helper()
	sub := &maasv1alpha1.MaaSSubscription{}
	key := types.NamespacedName{Namespace: ioNS, Name: ioSubName}
	if err := env.c.Get(context.Background(), key, sub); err != nil {
		t.Fatalf("Get subscription: %v", err)
	}
	if err := env.c.Delete(context.Background(), sub); err != nil {
		t.Fatalf("Delete subscription: %v", err)
	}
	_, err := env.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	return err
}

func subscriptionExists(t *testing.T, c client.Client) bool {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ioNS, Name: ioSubName}, &maasv1alpha1.MaaSSubscription{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("Get subscription: %v", err)
	}
	return err == nil
}

func TestHandleDeletion_DeletesObjectivesInAllNamespaces(t *testing.T) {
	sub := newPrioritySubscription(ptrInt32(1))
	sub.Finalizers = []string{maasSubscriptionFinalizer}
	inDefault := ownedObjective(sub, types.NamespacedName{Namespace: ioNS, Name: "obj-a"}, "pool-a", 1)
	inOther := ownedObjective(sub, types.NamespacedName{Namespace: "models", Name: "obj-b"}, "pool-b", 1)
	other := newPrioritySubscription(ptrInt32(1))
	other.Name = "silver"
	notOurs := ownedObjective(other, types.NamespacedName{Namespace: ioNS, Name: "obj-silver"}, "pool-a", 1)
	env := newIOEnv(t, nil, sub, inDefault, inOther, notOurs)

	if err := deleteSubscription(t, env); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := env.objectives(t)
	if len(got) != 1 {
		t.Errorf("objectives = %v, want only the other subscription's", got)
	}
	if _, ok := got[client.ObjectKeyFromObject(notOurs)]; !ok {
		t.Error("another subscription's objective was deleted")
	}
	if subscriptionExists(t, env.c) {
		t.Error("finalizer was not removed")
	}
}

func TestHandleDeletion_DeleteErrorKeepsFinalizer(t *testing.T) {
	sub := newPrioritySubscription(ptrInt32(1))
	sub.Finalizers = []string{maasSubscriptionFinalizer}
	funcs := &interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*llmdv1alpha2.InferenceObjective); ok {
				return errors.New("simulated delete failure")
			}
			return cl.Delete(ctx, obj, opts...)
		},
	}
	env := newIOEnv(t, funcs, sub, ownedObjective(sub, types.NamespacedName{Namespace: ioNS, Name: "obj-a"}, "pool-a", 1))

	if err := deleteSubscription(t, env); err == nil {
		t.Fatal("expected the delete error to be returned")
	}
	if !subscriptionExists(t, env.c) {
		t.Error("finalizer was removed although an objective could not be deleted")
	}
}

// TestHandleDeletion_ListsFromAPIReader verifies finalizer cleanup finds an objective the
// cache has not seen yet, so it is deleted rather than orphaned.
func TestHandleDeletion_ListsFromAPIReader(t *testing.T) {
	sub := newPrioritySubscription(ptrInt32(1))
	sub.Finalizers = []string{maasSubscriptionFinalizer}
	env := newIOEnv(t, nil, sub, ownedObjective(sub, types.NamespacedName{Namespace: ioNS, Name: "obj-a"}, "pool-a", 1))
	apiServer := env.c
	// The cached client has not observed the objective yet.
	env.r.Client = interceptor.NewClient(apiServer, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*llmdv1alpha2.InferenceObjectiveList); ok {
				return nil
			}
			return cl.List(ctx, list, opts...)
		},
	})

	if err := deleteSubscription(t, env); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := env.objectives(t); len(got) != 0 {
		t.Errorf("objectives = %v, want the objective the cache missed to be deleted", got)
	}
	if subscriptionExists(t, env.c) {
		t.Error("finalizer was not removed")
	}
}

func TestHandleDeletion_APIMissingRemovesFinalizer(t *testing.T) {
	sub := newPrioritySubscription(ptrInt32(1))
	sub.Finalizers = []string{maasSubscriptionFinalizer}
	env := newIOEnv(t, nil, sub)
	env.r.inferenceObjectivesUnavailable.Store(true)

	if err := deleteSubscription(t, env); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if subscriptionExists(t, env.c) {
		t.Error("finalizer was not removed")
	}
}

// TestHandleDeletion_NamespaceLostTenantLabel verifies cleanup runs even when the namespace
// is no longer a tenant namespace.
func TestHandleDeletion_NamespaceLostTenantLabel(t *testing.T) {
	sub := newPrioritySubscription(ptrInt32(1))
	sub.Finalizers = []string{maasSubscriptionFinalizer}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ioNS}}
	env := newIOEnv(t, nil, ns, sub, ownedObjective(sub, types.NamespacedName{Namespace: ioNS, Name: "obj-a"}, "pool-a", 1))
	env.r.DefaultTenantNamespace = "models-as-a-service"
	env.r.TenantNamespaceDiscoveryEnabled = true

	if err := deleteSubscription(t, env); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := env.objectives(t); len(got) != 0 {
		t.Errorf("objectives = %v, want none", got)
	}
	if subscriptionExists(t, env.c) {
		t.Error("finalizer was not removed")
	}
}

// --- Mappers and predicates ---

func TestMapInferenceObjectiveToSubscription(t *testing.T) {
	sub := newPrioritySubscription(ptrInt32(1))
	obj := ownedObjective(sub, types.NamespacedName{Namespace: "models", Name: "obj"}, "pool", 1)
	reqs := mapInferenceObjectiveToSubscription(context.Background(), obj)
	want := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ioNS, Name: ioSubName}}
	if len(reqs) != 1 || reqs[0] != want {
		t.Errorf("requests = %v, want [%v]", reqs, want)
	}

	obj.SetAnnotations(nil)
	if reqs := mapInferenceObjectiveToSubscription(context.Background(), obj); len(reqs) != 0 {
		t.Errorf("requests without the subscription annotation = %v, want none", reqs)
	}
}

func TestOwnedInferenceObjectivePredicate(t *testing.T) {
	p := ownedInferenceObjectivePredicate()
	sub := newPrioritySubscription(ptrInt32(1))
	owned := ownedObjective(sub, types.NamespacedName{Namespace: ioNS, Name: "obj"}, "pool", 1)
	owned.SetGeneration(1)
	foreign := &llmdv1alpha2.InferenceObjective{}

	if !p.Create(event.CreateEvent{Object: owned}) || p.Create(event.CreateEvent{Object: foreign}) {
		t.Error("Create should pass only owned objectives")
	}
	if !p.Delete(event.DeleteEvent{Object: owned}) || p.Delete(event.DeleteEvent{Object: foreign}) {
		t.Error("Delete should pass only owned objectives")
	}

	statusOnly := owned.DeepCopy()
	statusOnly.Status.Conditions = []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionTrue}}
	if p.Update(event.UpdateEvent{ObjectOld: owned, ObjectNew: statusOnly}) {
		t.Error("status-only update should be dropped")
	}
	specChange := owned.DeepCopy()
	specChange.SetGeneration(2)
	if !p.Update(event.UpdateEvent{ObjectOld: owned, ObjectNew: specChange}) {
		t.Error("generation change should pass")
	}
	labelsRemoved := owned.DeepCopy()
	labelsRemoved.SetLabels(nil)
	if !p.Update(event.UpdateEvent{ObjectOld: owned, ObjectNew: labelsRemoved}) {
		t.Error("removing ownership labels should pass so they are restored")
	}
	annotationChange := owned.DeepCopy()
	annotationChange.SetAnnotations(map[string]string{annotationSubscriptionName: "renamed"})
	if !p.Update(event.UpdateEvent{ObjectOld: owned, ObjectNew: annotationChange}) {
		t.Error("annotation change should pass")
	}
	foreignChange := foreign.DeepCopy()
	foreignChange.SetGeneration(5)
	if p.Update(event.UpdateEvent{ObjectOld: foreign, ObjectNew: foreignChange}) {
		t.Error("updates to foreign objects should be dropped")
	}
}

func TestMapTenantConfigToMaaSSubscriptions(t *testing.T) {
	inNS := newPrioritySubscription(ptrInt32(1))
	elsewhere := newPrioritySubscription(nil)
	elsewhere.Namespace = "other"
	env := newIOEnv(t, nil, inNS, elsewhere)
	config := &maasv1alpha1.MaasTenantConfig{ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: ioNS}}

	reqs := env.r.mapTenantConfigToMaaSSubscriptions(context.Background(), config)
	want := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ioNS, Name: ioSubName}}
	if len(reqs) != 1 || reqs[0] != want {
		t.Errorf("requests = %v, want [%v]", reqs, want)
	}
}

func TestSubscriptionsWithInferencePriority(t *testing.T) {
	withPriority := newPrioritySubscription(ptrInt32(0))
	without := newPrioritySubscription(nil)
	without.Name = "silver"
	env := newIOEnv(t, nil, withPriority, without)

	reqs, err := env.r.subscriptionsWithInferencePriority(context.Background())
	want := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ioNS, Name: ioSubName}}
	if err != nil || len(reqs) != 1 || reqs[0] != want {
		t.Errorf("requests = %v, %v; want [%v]", reqs, err, want)
	}

	failing := newIOEnv(t, &interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("simulated list failure")
		},
	})
	if _, err := failing.r.subscriptionsWithInferencePriority(context.Background()); err == nil {
		t.Error("list failure was not returned for retry")
	}
}

func TestMapLLMISvcToMaaSSubscriptions(t *testing.T) {
	gold := newPrioritySubscription(nil, "llama", "llama-2")
	silver := newPrioritySubscription(nil, "llama-2")
	silver.Name = "silver"
	bronze := newPrioritySubscription(nil, "other", "same-name-external")
	bronze.Name = "bronze"
	env := newIOEnv(t, nil,
		newMaaSModelRef("llama", ioNS, "LLMInferenceService", "llama-svc"),
		newMaaSModelRef("llama-2", ioNS, "LLMInferenceService", "llama-svc"),
		newMaaSModelRef("other", ioNS, "LLMInferenceService", "other-svc"),
		newMaaSModelRef("same-name-external", ioNS, "ExternalModel", "llama-svc"),
		gold, silver, bronze,
	)

	reqs := env.r.mapLLMISvcToMaaSSubscriptions(context.Background(), newLLMISvc("llama-svc", ioNS))
	got := map[string]bool{}
	for _, req := range reqs {
		if got[req.Name] {
			t.Errorf("duplicate request for %s", req.Name)
		}
		got[req.Name] = true
	}
	if len(got) != 2 || !got[ioSubName] || !got["silver"] {
		t.Errorf("requests = %v, want gold and silver", reqs)
	}
}

func TestLLMISvcRouterStatusChangedPredicate(t *testing.T) {
	p := llmisvcRouterStatusChangedPredicate{}
	base := newLLMISvcWithPool("svc", ioNS, "pool", "")

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

// --- CRD-established bootstrap ---

func crdWithEstablished(name string, status apiextensionsv1.ConditionStatus) *apiextensionsv1.CustomResourceDefinition {
	crd := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if status != "" {
		crd.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{{Type: apiextensionsv1.Established, Status: status}}
	}
	return crd
}

// testCRDBootstrap returns a bootstrap with millisecond retries whose register and hook
// fail the given number of times before succeeding.
func testCRDBootstrap(registerFailures, hookFailures int32, registered, hooked *atomic.Int32, requests []reconcile.Request) *crdBootstrap {
	b := newCRDBootstrap(inferenceObjectiveCRD,
		func() error {
			if registered.Add(1) <= registerFailures {
				return errors.New("simulated watch registration failure")
			}
			return nil
		},
		func(context.Context) ([]reconcile.Request, error) {
			if hooked.Add(1) <= hookFailures {
				return nil, errors.New("simulated list failure")
			}
			return requests, nil
		},
	)
	b.log = logr.Discard()
	b.initialDelay = time.Millisecond
	b.maxDelay = 4 * time.Millisecond
	return b
}

func waitForBootstrap(t *testing.T, b *crdBootstrap) {
	t.Helper()
	select {
	case <-b.done:
	case <-time.After(5 * time.Second):
		t.Fatal("bootstrap did not finish")
	}
}

func TestCRDBootstrap_WaitsForEstablished(t *testing.T) {
	var registered, hooked atomic.Int32
	want := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ioNS, Name: ioSubName}}
	b := testCRDBootstrap(0, 0, &registered, &hooked, []reconcile.Request{want})
	q := workqueue.NewTyped[reconcile.Request]()
	defer q.ShutDown()
	ctx := context.Background()

	b.handle(ctx, crdWithEstablished("other.example.com", apiextensionsv1.ConditionTrue), q)
	b.handle(ctx, crdWithEstablished(inferenceObjectiveCRD, ""), q)
	b.handle(ctx, crdWithEstablished(inferenceObjectiveCRD, apiextensionsv1.ConditionFalse), q)
	if b.started.Load() {
		t.Fatal("bootstrap started before the CRD was established")
	}

	b.handle(ctx, crdWithEstablished(inferenceObjectiveCRD, apiextensionsv1.ConditionTrue), q)
	waitForBootstrap(t, b)
	b.handle(ctx, crdWithEstablished(inferenceObjectiveCRD, apiextensionsv1.ConditionTrue), q)
	if registered.Load() != 1 || hooked.Load() != 1 {
		t.Errorf("registered=%d hooked=%d, want each once", registered.Load(), hooked.Load())
	}
	if q.Len() != 1 {
		t.Fatalf("queue has %d requests, want 1", q.Len())
	}
	if got, _ := q.Get(); got != want {
		t.Errorf("queued %v, want %v", got, want)
	}
}

// TestCRDBootstrap_RetriesFailures verifies a failed watch registration is retried before
// the hook runs, and a failed hook is retried until its requests are enqueued.
func TestCRDBootstrap_RetriesFailures(t *testing.T) {
	var registered, hooked atomic.Int32
	want := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ioNS, Name: ioSubName}}
	b := testCRDBootstrap(2, 3, &registered, &hooked, []reconcile.Request{want})
	b.register = func(inner func() error) func() error {
		return func() error {
			if hooked.Load() != 0 {
				t.Error("hook ran before the watch was registered")
			}
			return inner()
		}
	}(b.register)
	q := workqueue.NewTyped[reconcile.Request]()
	defer q.ShutDown()

	b.handle(context.Background(), crdWithEstablished(inferenceObjectiveCRD, apiextensionsv1.ConditionTrue), q)
	waitForBootstrap(t, b)
	if registered.Load() != 3 || hooked.Load() != 4 {
		t.Errorf("registered=%d hooked=%d, want 3 and 4 attempts", registered.Load(), hooked.Load())
	}
	if q.Len() != 1 {
		t.Errorf("queue has %d requests, want 1", q.Len())
	}
}

// TestCRDBootstrap_OutlivesEventContext verifies retries continue after the event
// handler's context is cancelled, as controller-runtime does when the handler returns.
func TestCRDBootstrap_OutlivesEventContext(t *testing.T) {
	var registered, hooked atomic.Int32
	b := testCRDBootstrap(1, 1, &registered, &hooked, nil)
	q := workqueue.NewTyped[reconcile.Request]()
	defer q.ShutDown()

	ctx, cancel := context.WithCancel(context.Background())
	b.handle(ctx, crdWithEstablished(inferenceObjectiveCRD, apiextensionsv1.ConditionTrue), q)
	cancel()
	waitForBootstrap(t, b)
	if registered.Load() != 2 || hooked.Load() != 2 {
		t.Errorf("registered=%d hooked=%d, want retries to continue after the handler returned", registered.Load(), hooked.Load())
	}
}
