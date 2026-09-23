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
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

const (
	unlimitedTestNamespace = "default"
	unlimitedTestModel     = "llm"
)

func TestMaaSSubscriptionReconciler_UnlimitedTRLPLimits(t *testing.T) {
	limitedKey := unlimitedTestNamespace + "-limited-" + unlimitedTestModel + "-tokens"

	tests := []struct {
		name string
		// The first subscription is the one reconciled.
		subs                   []*maasv1alpha1.MaaSSubscription
		wantKeys               []string
		wantUnlimitedPredicate string
		wantAnnotation         string
	}{
		{
			name:                   "unlimited next to limited adds one shared limit",
			subs:                   []*maasv1alpha1.MaaSSubscription{limitedTestSub("limited"), unlimitedTestSub("unl-a")},
			wantKeys:               []string{limitedKey, unlimitedLimitName},
			wantUnlimitedPredicate: unlimitedTestPredicate("unl-a"),
			wantAnnotation:         "default/limited,default/unl-a",
		},
		{
			name:                   "unlimited subscriptions share one limit",
			subs:                   []*maasv1alpha1.MaaSSubscription{limitedTestSub("limited"), unlimitedTestSub("unl-b"), unlimitedTestSub("unl-a")},
			wantKeys:               []string{limitedKey, unlimitedLimitName},
			wantUnlimitedPredicate: unlimitedTestPredicate("unl-a", "unl-b"),
			wantAnnotation:         "default/limited,default/unl-a,default/unl-b",
		},
		{
			name:                   "all unlimited keeps the TRLP non-empty",
			subs:                   []*maasv1alpha1.MaaSSubscription{unlimitedTestSub("unl-a")},
			wantKeys:               []string{unlimitedLimitName},
			wantUnlimitedPredicate: unlimitedTestPredicate("unl-a"),
			wantAnnotation:         "default/unl-a",
		},
		{
			name:           "ref without token budget is skipped",
			subs:           []*maasv1alpha1.MaaSSubscription{limitedTestSub("limited"), testSubWithRef("no-budget", maasv1alpha1.ModelSubscriptionRef{})},
			wantKeys:       []string{limitedKey},
			wantAnnotation: "default/limited",
		},
		{
			name:     "ref without token budget alone creates no TRLP",
			subs:     []*maasv1alpha1.MaaSSubscription{testSubWithRef("no-budget", maasv1alpha1.ModelSubscriptionRef{})},
			wantKeys: nil,
		},
		{
			name: "declared limits win over unlimited",
			subs: []*maasv1alpha1.MaaSSubscription{testSubWithRef("both", maasv1alpha1.ModelSubscriptionRef{
				Unlimited:       true,
				TokenRateLimits: []maasv1alpha1.TokenRateLimit{{Limit: 100, Window: "1m"}},
			})},
			wantKeys:       []string{unlimitedTestNamespace + "-both-" + unlimitedTestModel + "-tokens"},
			wantAnnotation: "default/both",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := make([]client.Object, 0, len(tt.subs))
			for _, s := range tt.subs {
				objs = append(objs, s)
			}
			c := newUnlimitedTestClient(objs...)

			reconcileTestSub(t, c, tt.subs[0].Name)

			if tt.wantKeys == nil {
				if _, err := getTestTRLP(t, c); !apierrors.IsNotFound(err) {
					t.Fatalf("expected no TokenRateLimitPolicy, got err=%v", err)
				}
				return
			}
			trlp, err := getTestTRLP(t, c)
			if err != nil {
				t.Fatalf("Get TokenRateLimitPolicy: %v", err)
			}

			limits := trlpLimits(t, trlp)
			gotKeys := getKeys(limits)
			slices.Sort(gotKeys)
			wantKeys := slices.Sorted(slices.Values(tt.wantKeys))
			if !slices.Equal(gotKeys, wantKeys) {
				t.Fatalf("limit keys = %v, want %v", gotKeys, wantKeys)
			}

			if got := trlp.GetAnnotations()["maas.opendatahub.io/subscriptions"]; got != tt.wantAnnotation {
				t.Errorf("subscriptions annotation = %q, want %q", got, tt.wantAnnotation)
			}

			if tt.wantUnlimitedPredicate == "" {
				return
			}
			entry, ok := limits[unlimitedLimitName].(map[string]any)
			if !ok {
				t.Fatalf("%s is not a map: %T", unlimitedLimitName, limits[unlimitedLimitName])
			}
			// No rates means Limitador enforces nothing and keeps no counters for
			// unlimited subscriptions; the limit only meters usage.
			if keys := getKeys(entry); !slices.Equal(keys, []string{"when"}) {
				t.Errorf("%s keys = %v, want only [when]", unlimitedLimitName, keys)
			}
			if got := firstPredicate(t, entry); got != tt.wantUnlimitedPredicate {
				t.Errorf("%s predicate = %q, want %q", unlimitedLimitName, got, tt.wantUnlimitedPredicate)
			}
		})
	}
}

// Adding an unlimited subscription must not touch the limits of limited ones,
// or it would rewrite their share of the gateway's WasmPlugin.
func TestMaaSSubscriptionReconciler_UnlimitedLeavesLimitedEntryUnchanged(t *testing.T) {
	limitedKey := unlimitedTestNamespace + "-limited-" + unlimitedTestModel + "-tokens"

	renderLimitedEntry := func(subs ...client.Object) string {
		t.Helper()
		c := newUnlimitedTestClient(subs...)
		reconcileTestSub(t, c, "limited")
		trlp, err := getTestTRLP(t, c)
		if err != nil {
			t.Fatalf("Get TokenRateLimitPolicy: %v", err)
		}
		out, err := json.Marshal(trlpLimits(t, trlp)[limitedKey])
		if err != nil {
			t.Fatalf("marshal limit entry: %v", err)
		}
		return string(out)
	}

	alone := renderLimitedEntry(limitedTestSub("limited"))
	mixed := renderLimitedEntry(limitedTestSub("limited"), unlimitedTestSub("unl-a"), unlimitedTestSub("unl-b"))
	if alone != mixed {
		t.Errorf("limited entry changed when unlimited subscriptions were added:\nalone: %s\nmixed: %s", alone, mixed)
	}
}

func TestMaaSSubscriptionReconciler_UnlimitedTRLPReconcileIsNoOp(t *testing.T) {
	c := newUnlimitedTestClient(limitedTestSub("limited"), unlimitedTestSub("unl-b"), unlimitedTestSub("unl-a"))

	reconcileTestSub(t, c, "limited")
	first, err := getTestTRLP(t, c)
	if err != nil {
		t.Fatalf("Get TokenRateLimitPolicy: %v", err)
	}

	reconcileTestSub(t, c, "unl-a")
	reconcileTestSub(t, c, "limited")
	second, err := getTestTRLP(t, c)
	if err != nil {
		t.Fatalf("Get TokenRateLimitPolicy: %v", err)
	}

	if first.GetResourceVersion() != second.GetResourceVersion() {
		t.Errorf("TokenRateLimitPolicy updated on an unchanged reconcile: resourceVersion %s -> %s",
			first.GetResourceVersion(), second.GetResourceVersion())
	}
}

func TestMaaSSubscriptionReconciler_UnlimitedSubscriptionDropsModel(t *testing.T) {
	const otherModel = "other"

	c := newUnlimitedTestClient(
		unlimitedTestSub("unl-a"),
		newMaaSModelRef(otherModel, unlimitedTestNamespace, "ExternalModel", otherModel),
		newHTTPRoute("maas-"+otherModel, unlimitedTestNamespace),
	)
	reconcileTestSub(t, c, "unl-a")
	if _, err := getTestTRLP(t, c); err != nil {
		t.Fatalf("Get TokenRateLimitPolicy: %v", err)
	}

	sub := &maasv1alpha1.MaaSSubscription{}
	if err := c.Get(t.Context(), types.NamespacedName{Name: "unl-a", Namespace: unlimitedTestNamespace}, sub); err != nil {
		t.Fatalf("Get MaaSSubscription: %v", err)
	}
	sub.Spec.ModelRefs[0].Name = otherModel
	if err := c.Update(t.Context(), sub); err != nil {
		t.Fatalf("Update MaaSSubscription: %v", err)
	}
	reconcileTestSub(t, c, "unl-a")

	if _, err := getTestTRLP(t, c); !apierrors.IsNotFound(err) {
		t.Errorf("expected the TokenRateLimitPolicy of the dropped model to be deleted, got err=%v", err)
	}
}

func TestMaaSSubscriptionReconciler_ToggleUnlimited(t *testing.T) {
	limitedKey := unlimitedTestNamespace + "-sub-" + unlimitedTestModel + "-tokens"
	c := newUnlimitedTestClient(limitedTestSub("sub"))

	setRef := func(ref maasv1alpha1.ModelSubscriptionRef) {
		t.Helper()
		sub := &maasv1alpha1.MaaSSubscription{}
		if err := c.Get(t.Context(), types.NamespacedName{Name: "sub", Namespace: unlimitedTestNamespace}, sub); err != nil {
			t.Fatalf("Get MaaSSubscription: %v", err)
		}
		ref.Name, ref.Namespace = unlimitedTestModel, unlimitedTestNamespace
		sub.Spec.ModelRefs = []maasv1alpha1.ModelSubscriptionRef{ref}
		if err := c.Update(t.Context(), sub); err != nil {
			t.Fatalf("Update MaaSSubscription: %v", err)
		}
	}
	assertKeys := func(want ...string) {
		t.Helper()
		reconcileTestSub(t, c, "sub")
		trlp, err := getTestTRLP(t, c)
		if err != nil {
			t.Fatalf("Get TokenRateLimitPolicy: %v", err)
		}
		if got := getKeys(trlpLimits(t, trlp)); !slices.Equal(got, want) {
			t.Fatalf("limit keys = %v, want %v", got, want)
		}
	}

	assertKeys(limitedKey)
	setRef(maasv1alpha1.ModelSubscriptionRef{Unlimited: true})
	assertKeys(unlimitedLimitName)
	setRef(maasv1alpha1.ModelSubscriptionRef{TokenRateLimits: []maasv1alpha1.TokenRateLimit{{Limit: 100, Window: "1m"}}})
	assertKeys(limitedKey)
}

// Every modelRef still gets a TRLP, so an all-unlimited subscription is not
// reported Degraded for a missing policy.
func TestMaaSSubscriptionReconciler_AllUnlimited_ActivePhase(t *testing.T) {
	existingTRLP := newPreexistingTRLP("maas-trlp-"+unlimitedTestModel, unlimitedTestNamespace, unlimitedTestModel, nil)
	if err := unstructured.SetNestedSlice(existingTRLP.Object, []any{
		map[string]any{"type": "Accepted", "status": "True"},
	}, "status", "conditions"); err != nil {
		t.Fatalf("SetNestedSlice status.conditions: %v", err)
	}
	c := newUnlimitedTestClient(unlimitedTestSub("unl-a"), existingTRLP)

	reconcileTestSub(t, c, "unl-a")

	var sub maasv1alpha1.MaaSSubscription
	if err := c.Get(t.Context(), types.NamespacedName{Name: "unl-a", Namespace: unlimitedTestNamespace}, &sub); err != nil {
		t.Fatalf("Get MaaSSubscription: %v", err)
	}
	if sub.Status.Phase != maasv1alpha1.PhaseActive {
		t.Errorf("phase = %q, want %q (message: %v)", sub.Status.Phase, maasv1alpha1.PhaseActive, sub.Status.Conditions)
	}
}

// The predicate must not depend on the order subscriptions are listed in, or
// the TRLP would be rewritten between reconciles.
func TestUnlimitedTokenLimit_PredicateOrderIsStable(t *testing.T) {
	key := func(sub string) string {
		return fmt.Sprintf("%s/%s@%s/%s", unlimitedTestNamespace, sub, unlimitedTestNamespace, unlimitedTestModel)
	}

	got := firstPredicate(t, unlimitedTokenLimit([]string{key("unl-b"), key("unl-a")}))

	if want := unlimitedTestPredicate("unl-a", "unl-b"); got != want {
		t.Errorf("predicate = %q, want %q", got, want)
	}
}

func unlimitedTestPredicate(subs ...string) string {
	matches := make([]string, 0, len(subs))
	for _, s := range subs {
		matches = append(matches, fmt.Sprintf(`auth.identity.selected_subscription_key == "%s/%s@%s/%s"`,
			unlimitedTestNamespace, s, unlimitedTestNamespace, unlimitedTestModel))
	}
	return fmt.Sprintf(`(%s) && !request.path.endsWith("/v1/models")`, strings.Join(matches, " || "))
}

func limitedTestSub(name string) *maasv1alpha1.MaaSSubscription {
	return newMaaSSubscription(name, unlimitedTestNamespace, "team-"+name, unlimitedTestModel, 100)
}

func unlimitedTestSub(name string) *maasv1alpha1.MaaSSubscription {
	return testSubWithRef(name, maasv1alpha1.ModelSubscriptionRef{Unlimited: true})
}

func testSubWithRef(name string, ref maasv1alpha1.ModelSubscriptionRef) *maasv1alpha1.MaaSSubscription {
	ref.Name, ref.Namespace = unlimitedTestModel, unlimitedTestNamespace
	return &maasv1alpha1.MaaSSubscription{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: unlimitedTestNamespace},
		Spec: maasv1alpha1.MaaSSubscriptionSpec{
			Owner:     maasv1alpha1.OwnerSpec{Groups: []maasv1alpha1.GroupReference{{Name: "team-" + name}}},
			ModelRefs: []maasv1alpha1.ModelSubscriptionRef{ref},
		},
	}
}

func newUnlimitedTestClient(objs ...client.Object) client.Client {
	base := []client.Object{
		newMaaSModelRef(unlimitedTestModel, unlimitedTestNamespace, "ExternalModel", unlimitedTestModel),
		newHTTPRoute("maas-"+unlimitedTestModel, unlimitedTestNamespace),
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(testRESTMapper()).
		WithObjects(append(base, objs...)...).
		WithStatusSubresource(&maasv1alpha1.MaaSSubscription{}).
		WithIndex(&maasv1alpha1.MaaSSubscription{}, "spec.modelRef", subscriptionModelRefIndexer).
		Build()
}

func reconcileTestSub(t *testing.T, c client.Client, name string) {
	t.Helper()
	r := &MaaSSubscriptionReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: unlimitedTestNamespace}}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("Reconcile %s: unexpected error: %v", name, err)
	}
}

func getTestTRLP(t *testing.T, c client.Client) (*unstructured.Unstructured, error) {
	t.Helper()
	trlp := &unstructured.Unstructured{}
	trlp.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1alpha1", Kind: "TokenRateLimitPolicy"})
	err := c.Get(t.Context(), types.NamespacedName{Name: "maas-trlp-" + unlimitedTestModel, Namespace: unlimitedTestNamespace}, trlp)
	return trlp, err
}

func trlpLimits(t *testing.T, trlp *unstructured.Unstructured) map[string]any {
	t.Helper()
	limits, found, err := unstructured.NestedMap(trlp.Object, "spec", "limits")
	if err != nil || !found {
		t.Fatalf("spec.limits not found: found=%v err=%v", found, err)
	}
	return limits
}

func firstPredicate(t *testing.T, limit map[string]any) string {
	t.Helper()
	when, found, err := unstructured.NestedSlice(limit, "when")
	if err != nil || !found || len(when) != 1 {
		t.Fatalf("expected a single when predicate: found=%v err=%v when=%v", found, err, when)
	}
	pred, _ := when[0].(map[string]any)["predicate"].(string)
	return pred
}
