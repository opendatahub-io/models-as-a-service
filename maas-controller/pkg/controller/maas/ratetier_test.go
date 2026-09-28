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
	"encoding/json"
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

func TestRateGroupKey(t *testing.T) {
	tests := []struct {
		name  string
		rates []any
		want  string
	}{
		{
			name:  "single rate",
			rates: []any{map[string]any{"limit": int64(1000), "window": "1h"}},
			want:  "1000/1h",
		},
		{
			name: "sorted lexicographically",
			rates: []any{
				map[string]any{"limit": int64(99999), "window": "1s"},
				map[string]any{"limit": int64(1000), "window": "1h"},
			},
			want: "1000/1h,99999/1s",
		},
		{
			name: "input order does not matter",
			rates: []any{
				map[string]any{"limit": int64(1000), "window": "1h"},
				map[string]any{"limit": int64(99999), "window": "1s"},
			},
			want: "1000/1h,99999/1s",
		},
		{
			name: "duplicates collapse",
			rates: []any{
				map[string]any{"limit": int64(100), "window": "1m"},
				map[string]any{"limit": int64(100), "window": "1m"},
			},
			want: "100/1m",
		},
		{
			name:  "windows are not normalized",
			rates: []any{map[string]any{"limit": int64(100), "window": "60s"}},
			want:  "100/60s",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := rateGroupKey(tc.rates); got != tc.want {
				t.Errorf("rateGroupKey(%v) = %q, want %q", tc.rates, got, tc.want)
			}
		})
	}
}

func TestRateGroupLimitName(t *testing.T) {
	tests := []struct{ key, want string }{
		{"1000/1h", "tokens-1000-per-1h"},
		{"1000/1h,99999/1s", "tokens-1000-per-1h-99999-per-1s"},
	}
	for _, tc := range tests {
		if got := rateGroupLimitName(tc.key); got != tc.want {
			t.Errorf("rateGroupLimitName(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// TestMaaSSubscriptionReconciler_GroupByIdenticalRate is the core regression
// test for RHOAIENG-95277: N subscriptions sharing a rate must collapse into
// one TokenRateLimitPolicy limit instead of one each.
func TestMaaSSubscriptionReconciler_GroupByIdenticalRate(t *testing.T) {
	const (
		modelName     = "llm"
		namespace     = "default"
		httpRouteName = "maas-" + modelName
		trlpName      = "maas-trlp-" + modelName
	)

	model := newMaaSModelRef(modelName, namespace, "ExternalModel", modelName)
	route := newHTTPRoute(httpRouteName, namespace)
	// Three subscriptions at the identical rate, one at a different rate.
	subA := newMaaSSubscription("sub-a", namespace, "team-a", modelName, 500)
	subB := newMaaSSubscription("sub-b", namespace, "team-b", modelName, 500)
	subC := newMaaSSubscription("sub-c", namespace, "team-c", modelName, 500)
	subD := newMaaSSubscription("sub-d", namespace, "team-d", modelName, 900)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(model, route, subA, subB, subC, subD).
		WithStatusSubresource(&maasv1alpha1.MaaSSubscription{}).
		WithIndex(&maasv1alpha1.MaaSSubscription{}, "spec.modelRef", subscriptionModelRefIndexer).
		Build()

	r := &MaaSSubscriptionReconciler{Client: c, Scheme: scheme}
	for _, name := range []string{"sub-a", "sub-b", "sub-c", "sub-d"} {
		req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}
		if _, err := r.Reconcile(t.Context(), req); err != nil {
			t.Fatalf("Reconcile %s: %v", name, err)
		}
	}

	trlp := &unstructured.Unstructured{}
	trlp.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})
	if err := c.Get(t.Context(), types.NamespacedName{Name: trlpName, Namespace: namespace}, trlp); err != nil {
		t.Fatalf("Get TokenRateLimitPolicy: %v", err)
	}
	limitsMap, found, err := unstructured.NestedMap(trlp.Object, "spec", "limits")
	if err != nil || !found {
		t.Fatalf("spec.limits not found: found=%v err=%v", found, err)
	}

	// Two distinct rates -> two limits, not four.
	if len(limitsMap) != 2 {
		t.Fatalf("expected 2 limits (grouped by rate), got %d: %v", len(limitsMap), getKeys(limitsMap))
	}

	shared, ok := limitsMap["tokens-500-per-1m"].(map[string]any)
	if !ok {
		t.Fatalf("expected grouped limit %q not found, got keys: %v", "tokens-500-per-1m", getKeys(limitsMap))
	}
	whenSlice, _, _ := unstructured.NestedSlice(shared, "when")
	pred := predicateOf(t, whenSlice)

	for _, sub := range []string{"sub-a", "sub-b", "sub-c"} {
		clause := fmt.Sprintf(`auth.identity.selected_subscription_key == "%s/%s@%s/%s"`, namespace, sub, namespace, modelName)
		if !containsString(pred, clause) {
			t.Errorf("grouped predicate is missing %s's clause: %s", sub, pred)
		}
	}
	if containsString(pred, "sub-d") {
		t.Errorf("grouped predicate must not reference sub-d (different rate): %s", pred)
	}

	counters, _, _ := unstructured.NestedSlice(shared, "counters")
	if len(counters) != 2 {
		t.Fatalf("expected 2 counters, got %d: %v", len(counters), counters)
	}
	c0, ok0 := counters[0].(map[string]any)
	c1, ok1 := counters[1].(map[string]any)
	if !ok0 || !ok1 || c0["expression"] != "auth.identity.selected_subscription_key" ||
		c1["expression"] != "auth.identity.userid" {
		t.Errorf("counters = %v, want [selected_subscription_key, userid]", counters)
	}

	// sub-d kept its own limit at its own rate.
	if _, ok := limitsMap["tokens-900-per-1m"]; !ok {
		t.Errorf("expected sub-d's own limit %q, got keys: %v", "tokens-900-per-1m", getKeys(limitsMap))
	}
}

// TestMaaSSubscriptionReconciler_RateEditMovesGroup verifies that changing a
// subscription's rate moves it into a different limit's predicate rather than
// leaving a stale clause behind, and that a second reconcile with no changes
// is a no-op.
func TestMaaSSubscriptionReconciler_RateEditMovesGroup(t *testing.T) {
	const (
		modelName     = "llm"
		namespace     = "default"
		httpRouteName = "maas-" + modelName
		trlpName      = "maas-trlp-" + modelName
	)

	model := newMaaSModelRef(modelName, namespace, "ExternalModel", modelName)
	route := newHTTPRoute(httpRouteName, namespace)
	subA := newMaaSSubscription("sub-a", namespace, "team-a", modelName, 500)
	subB := newMaaSSubscription("sub-b", namespace, "team-b", modelName, 500)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(model, route, subA, subB).
		WithStatusSubresource(&maasv1alpha1.MaaSSubscription{}).
		WithIndex(&maasv1alpha1.MaaSSubscription{}, "spec.modelRef", subscriptionModelRefIndexer).
		Build()

	r := &MaaSSubscriptionReconciler{Client: c, Scheme: scheme}
	reqA := ctrl.Request{NamespacedName: types.NamespacedName{Name: "sub-a", Namespace: namespace}}
	reqB := ctrl.Request{NamespacedName: types.NamespacedName{Name: "sub-b", Namespace: namespace}}
	for _, req := range []ctrl.Request{reqA, reqB} {
		if _, err := r.Reconcile(t.Context(), req); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}

	get := func() *unstructured.Unstructured {
		trlp := &unstructured.Unstructured{}
		trlp.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})
		if err := c.Get(t.Context(), types.NamespacedName{Name: trlpName, Namespace: namespace}, trlp); err != nil {
			t.Fatalf("Get TokenRateLimitPolicy: %v", err)
		}
		return trlp
	}

	before := get()
	beforeLimits, _, _ := unstructured.NestedMap(before.Object, "spec", "limits")
	if len(beforeLimits) != 1 {
		t.Fatalf("expected 1 grouped limit before the edit, got %d: %v", len(beforeLimits), getKeys(beforeLimits))
	}

	// Reconciling sub-a again with no spec change must be a no-op.
	if _, err := r.Reconcile(t.Context(), reqA); err != nil {
		t.Fatalf("Reconcile (no-op): %v", err)
	}
	afterNoop := get()
	if afterNoop.GetResourceVersion() != before.GetResourceVersion() {
		t.Errorf("no-op reconcile changed resourceVersion: %s -> %s", before.GetResourceVersion(), afterNoop.GetResourceVersion())
	}

	// Edit sub-a's rate so it no longer matches sub-b's.
	var sub maasv1alpha1.MaaSSubscription
	if err := c.Get(t.Context(), types.NamespacedName{Name: "sub-a", Namespace: namespace}, &sub); err != nil {
		t.Fatalf("Get sub-a: %v", err)
	}
	sub.Spec.ModelRefs[0].TokenRateLimits[0].Limit = 700
	if err := c.Update(t.Context(), &sub); err != nil {
		t.Fatalf("Update sub-a: %v", err)
	}
	if _, err := r.Reconcile(t.Context(), reqA); err != nil {
		t.Fatalf("Reconcile after rate edit: %v", err)
	}

	after := get()
	afterLimits, _, _ := unstructured.NestedMap(after.Object, "spec", "limits")
	if len(afterLimits) != 2 {
		t.Fatalf("expected 2 limits after the rate diverges, got %d: %v", len(afterLimits), getKeys(afterLimits))
	}
	movedLimit, ok := afterLimits["tokens-700-per-1m"].(map[string]any)
	if !ok {
		t.Fatalf("expected sub-a's new limit %q, got keys: %v", "tokens-700-per-1m", getKeys(afterLimits))
	}
	movedWhen, _, _ := unstructured.NestedSlice(movedLimit, "when")
	movedPred := predicateOf(t, movedWhen)
	if containsString(movedPred, "sub-b") {
		t.Errorf("sub-a's new limit must not reference sub-b: %s", movedPred)
	}

	oldGroup, ok := afterLimits["tokens-500-per-1m"].(map[string]any)
	if !ok {
		t.Fatalf("expected sub-b's limit %q to remain, got keys: %v", "tokens-500-per-1m", getKeys(afterLimits))
	}
	oldWhen, _, _ := unstructured.NestedSlice(oldGroup, "when")
	oldPred := predicateOf(t, oldWhen)
	if containsString(oldPred, "sub-a") {
		t.Errorf("sub-a's old clause must be gone from the 500/1m limit: %s", oldPred)
	}
	if !containsString(oldPred, "sub-b") {
		t.Errorf("sub-b's clause must remain in the 500/1m limit: %s", oldPred)
	}
}

// TestMaaSSubscriptionReconciler_GroupingGrowsWithDistinctRates is the size
// guard for RHOAIENG-95277: the number of limits must depend on the number of
// distinct rates, not on the number of subscriptions, and the marshalled size
// must grow only by the added predicate clauses - not by repeating the fixed
// per-limit fields (rates/counters/service) once per subscription.
func TestMaaSSubscriptionReconciler_GroupingGrowsWithDistinctRates(t *testing.T) {
	const (
		modelName     = "llm"
		namespace     = "default"
		httpRouteName = "maas-" + modelName
		trlpName      = "maas-trlp-" + modelName
	)
	rates := []int64{500, 700, 900}

	build := func(t *testing.T, subCount int) *unstructured.Unstructured {
		t.Helper()
		model := newMaaSModelRef(modelName, namespace, "ExternalModel", modelName)
		route := newHTTPRoute(httpRouteName, namespace)
		objs := []client.Object{model, route}
		var names []string
		for i := 0; i < subCount; i++ {
			name := fmt.Sprintf("sub-%03d", i)
			names = append(names, name)
			objs = append(objs, newMaaSSubscription(name, namespace, fmt.Sprintf("team-%d", i), modelName, rates[i%len(rates)]))
		}

		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRESTMapper(testRESTMapper()).
			WithObjects(objs...).
			WithStatusSubresource(&maasv1alpha1.MaaSSubscription{}).
			WithIndex(&maasv1alpha1.MaaSSubscription{}, "spec.modelRef", subscriptionModelRefIndexer).
			Build()
		r := &MaaSSubscriptionReconciler{Client: c, Scheme: scheme}
		for _, name := range names {
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}
			if _, err := r.Reconcile(t.Context(), req); err != nil {
				t.Fatalf("Reconcile %s: %v", name, err)
			}
		}

		trlp := &unstructured.Unstructured{}
		trlp.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})
		if err := c.Get(t.Context(), types.NamespacedName{Name: trlpName, Namespace: namespace}, trlp); err != nil {
			t.Fatalf("Get TokenRateLimitPolicy: %v", err)
		}
		return trlp
	}

	few := build(t, 3)
	many := build(t, 60)

	fewLimits, _, _ := unstructured.NestedMap(few.Object, "spec", "limits")
	manyLimits, _, _ := unstructured.NestedMap(many.Object, "spec", "limits")
	if len(fewLimits) != len(rates) || len(manyLimits) != len(rates) {
		t.Fatalf("expected %d limits regardless of subscription count, got %d (3 subs) and %d (60 subs)",
			len(rates), len(fewLimits), len(manyLimits))
	}

	fewSpec, _, _ := unstructured.NestedMap(few.Object, "spec")
	manySpec, _, _ := unstructured.NestedMap(many.Object, "spec")
	fewJSON, err := json.Marshal(fewSpec)
	if err != nil {
		t.Fatalf("marshal fewSpec: %v", err)
	}
	manyJSON, err := json.Marshal(manySpec)
	if err != nil {
		t.Fatalf("marshal manySpec: %v", err)
	}

	// Only the OR-clauses for the 57 extra subscriptions should account for the
	// size difference. One clause is `auth.identity.selected_subscription_key ==
	// "default/sub-XXX@default/llm" || `, roughly 90-140 bytes; give generous
	// headroom (200 B/clause) so this catches a regression back to one full
	// limit per subscription (which costs several hundred bytes of fixed
	// overhead per subscription on top of the clause) without being flaky.
	grew := len(manyJSON) - len(fewJSON)
	extraSubs := 60 - 3
	maxExpected := extraSubs * 200
	t.Logf("spec grew by %d bytes for %d extra subscriptions (%.1f B/sub)", grew, extraSubs, float64(grew)/float64(extraSubs))
	if grew <= 0 {
		t.Fatalf("60 subscriptions produced a spec no bigger than 3 (%d vs %d bytes) - grouping key must be wrong", len(manyJSON), len(fewJSON))
	}
	if grew > maxExpected {
		t.Errorf("spec grew by %d bytes for %d extra subscriptions (%.0f B/sub), expected under %d B/sub - "+
			"looks like a full limit is being paid per subscription again, not just a predicate clause",
			grew, extraSubs, float64(grew)/float64(extraSubs), 200)
	}
}

// predicateOf extracts the single "when[0].predicate" string a limit built by
// reconcileTRLPForModel always has, failing the test on any shape mismatch.
func predicateOf(t *testing.T, whenSlice []any) string {
	t.Helper()
	if len(whenSlice) != 1 {
		t.Fatalf("expected 1 when predicate, got %d", len(whenSlice))
	}
	predMap, ok := whenSlice[0].(map[string]any)
	if !ok {
		t.Fatalf("whenSlice[0] is not map[string]any: %T", whenSlice[0])
	}
	pred, ok := predMap["predicate"].(string)
	if !ok {
		t.Fatalf("predicate is not a string: %T", predMap["predicate"])
	}
	return pred
}

// containsString and getKeys are defined in maassubscription_controller_test.go.

// TestBuildGroupedLimits_UnlimitedNextToGroups covers a model carrying both
// kinds of subscription: same-rate ones collapse into one rate group, unlimited
// ones share the rate-less unlimitedLimitName limit, and every subscription is
// listed in the tracking annotation.
func TestBuildGroupedLimits_UnlimitedNextToGroups(t *testing.T) {
	rated := []any{map[string]any{"limit": int64(500), "window": "1m"}}
	sub := func(name string, unlimited bool) subInfo {
		si := subInfo{
			subNamespace: "ns",
			subName:      name,
			unlimited:    unlimited,
			modelScoped:  "ns/" + name + "@models/llm",
		}
		if !unlimited {
			si.rates = rated
			si.groupKey = rateGroupKey(rated)
		}
		return si
	}

	limits, names := buildGroupedLimits([]subInfo{
		sub("b-rated", false), sub("z-free", true), sub("a-rated", false), sub("y-free", true),
	})

	if got := getKeys(limits); len(got) != 2 {
		t.Fatalf("expected one rate group and one unlimited limit, got %v", got)
	}
	grouped, ok := limits["tokens-500-per-1m"].(map[string]any)
	if !ok {
		t.Fatalf("rate group %q missing, got %v", "tokens-500-per-1m", getKeys(limits))
	}
	wantGrouped := `(auth.identity.selected_subscription_key == "ns/a-rated@models/llm" || ` +
		`auth.identity.selected_subscription_key == "ns/b-rated@models/llm") && !request.path.endsWith("/v1/models")`
	groupedWhen, _, _ := unstructured.NestedSlice(grouped, "when")
	if got := predicateOf(t, groupedWhen); got != wantGrouped {
		t.Errorf("rate group predicate = %q, want %q", got, wantGrouped)
	}

	free, ok := limits[unlimitedLimitName].(map[string]any)
	if !ok {
		t.Fatalf("unlimited limit %q missing, got %v", unlimitedLimitName, getKeys(limits))
	}
	wantFree := `(auth.identity.selected_subscription_key == "ns/y-free@models/llm" || ` +
		`auth.identity.selected_subscription_key == "ns/z-free@models/llm") && !request.path.endsWith("/v1/models")`
	freeWhen, _, _ := unstructured.NestedSlice(free, "when")
	if got := predicateOf(t, freeWhen); got != wantFree {
		t.Errorf("unlimited predicate = %q, want %q", got, wantFree)
	}
	if _, hasRates := free["rates"]; hasRates {
		t.Errorf("unlimited limit must not carry rates: %v", free)
	}

	wantNames := []string{"ns/a-rated", "ns/b-rated", "ns/y-free", "ns/z-free"}
	if len(names) != len(wantNames) {
		t.Fatalf("annotation names = %v, want %v", names, wantNames)
	}
	for i := range wantNames {
		if names[i] != wantNames[i] {
			t.Errorf("annotation names = %v, want %v", names, wantNames)
			break
		}
	}
}

// TestBuildGroupLimit_RatesRenderedCanonically covers members that list the
// same rate set in different orders, with a duplicate: whichever member comes
// first, the rendered limit must be identical, or List ordering between
// reconciles would flip the spec and force needless policy updates.
func TestBuildGroupLimit_RatesRenderedCanonically(t *testing.T) {
	hour := map[string]any{"limit": int64(1000), "window": "1h"}
	sec := map[string]any{"limit": int64(99999), "window": "1s"}
	a := subInfo{modelScoped: "ns/a@models/llm", rates: []any{sec, hour}}
	b := subInfo{modelScoped: "ns/b@models/llm", rates: []any{hour, sec, hour}}
	if rateGroupKey(a.rates) != rateGroupKey(b.rates) {
		t.Fatalf("members must share a group: %q vs %q", rateGroupKey(a.rates), rateGroupKey(b.rates))
	}

	ab, err := json.Marshal(buildGroupLimit([]subInfo{a, b}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ba, err := json.Marshal(buildGroupLimit([]subInfo{b, a}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(ab) != string(ba) {
		t.Errorf("limit depends on member order:\n%s\n%s", ab, ba)
	}

	rates, _, _ := unstructured.NestedSlice(buildGroupLimit([]subInfo{b, a}), "rates")
	want := []map[string]any{hour, sec}
	if len(rates) != len(want) {
		t.Fatalf("rates = %v, want %v (deduped, sorted)", rates, want)
	}
	for i := range want {
		got, ok := rates[i].(map[string]any)
		if !ok || got["limit"] != want[i]["limit"] || got["window"] != want[i]["window"] {
			t.Errorf("rates = %v, want %v (deduped, sorted)", rates, want)
			break
		}
	}
}
