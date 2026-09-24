package subscription_test

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
)

func TestSelector_UnlimitedModelRef(t *testing.T) {
	log := logger.New(false)
	limits := []any{map[string]any{"limit": int64(100), "window": "1m"}}

	tests := []struct {
		name          string
		ref           map[string]any
		wantUnlimited bool
		wantLimits    int
	}{
		{
			name:          "unlimited ref",
			ref:           map[string]any{"name": "model-a", "namespace": "ns", "unlimited": true},
			wantUnlimited: true,
		},
		{
			name:       "limited ref",
			ref:        map[string]any{"name": "model-a", "namespace": "ns", "tokenRateLimits": limits},
			wantLimits: 1,
		},
		{
			// Mirrors the controller, which enforces the declared limits when a ref
			// bypasses the CRD rule that makes the two fields mutually exclusive.
			name:       "declared limits win over unlimited",
			ref:        map[string]any{"name": "model-a", "namespace": "ns", "unlimited": true, "tokenRateLimits": limits},
			wantLimits: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lister := &fakeLister{subscriptions: []*unstructured.Unstructured{
				createSubscriptionWithModelRefs("sub", []string{"g1"}, []map[string]any{tt.ref}),
			}}
			sel := subscription.NewSelector(log, lister, nil, nil)

			got, err := sel.GetAllAccessible([]string{"g1"}, "")
			if err != nil {
				t.Fatalf("GetAllAccessible: %v", err)
			}
			if len(got) != 1 || len(got[0].ModelRefs) != 1 {
				t.Fatalf("expected one subscription with one model ref, got %+v", got)
			}
			ref := got[0].ModelRefs[0]
			if ref.Unlimited != tt.wantUnlimited {
				t.Errorf("Unlimited = %v, want %v", ref.Unlimited, tt.wantUnlimited)
			}
			if len(ref.TokenRateLimits) != tt.wantLimits {
				t.Errorf("TokenRateLimits = %v, want %d entries", ref.TokenRateLimits, tt.wantLimits)
			}
		})
	}
}

func TestSelectHighestPriority_UnlimitedWinsMaxLimitTieBreak(t *testing.T) {
	log := logger.New(false)
	lister := &fakeLister{subscriptions: []*unstructured.Unstructured{
		// Same priority as createSubscriptionWithModelRefs; the name tie-break
		// alone would pick a-limited.
		createSubscription("a-limited", []string{"g1"}, nil, 10, 1_000_000_000, "", ""),
		createSubscriptionWithModelRefs("b-unlimited", []string{"g1"}, []map[string]any{
			{"name": "test-model", "namespace": "ns", "unlimited": true},
		}),
	}}
	sel := subscription.NewSelector(log, lister, nil, nil)

	got, err := sel.SelectHighestPriority([]string{"g1"}, "")
	if err != nil {
		t.Fatalf("SelectHighestPriority: %v", err)
	}
	if got.Name != "b-unlimited" {
		t.Errorf("expected b-unlimited (unlimited outranks any limit), got %q", got.Name)
	}
}

// An unlimited model still depends on its TRLP: without it the gateway default
// deny applies, so a Degraded subscription must not route there blind.
func TestSelector_DegradedUnlimitedModelRequiresReadyTRLP(t *testing.T) {
	log := logger.Production()

	tests := []struct {
		name       string
		trlpStatus []any
		wantReason string
	}{
		{
			name:       "TRLP ready allows inference",
			trlpStatus: []any{map[string]any{"model": "model-a", "name": "maas-trlp-model-a", "namespace": "ns", "ready": true}},
		},
		{
			name:       "TRLP not ready blocks inference",
			trlpStatus: []any{map[string]any{"model": "model-a", "name": "maas-trlp-model-a", "namespace": "ns", "ready": false}},
			wantReason: "RateLimitNotEnforced",
		},
		{
			name:       "TRLP status missing blocks inference",
			wantReason: "RateLimitNotEnforced",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub := createSubscriptionWithModelRefs("degraded-sub", []string{"g1"}, []map[string]any{
				{"name": "model-a", "namespace": "ns", "unlimited": true},
			})
			if err := unstructured.SetNestedField(sub.Object, phaseDegraded, "status", "phase"); err != nil {
				t.Fatalf("set status.phase: %v", err)
			}
			if tt.trlpStatus != nil {
				if err := unstructured.SetNestedSlice(sub.Object, tt.trlpStatus, "status", "tokenRateLimitStatuses"); err != nil {
					t.Fatalf("set status.tokenRateLimitStatuses: %v", err)
				}
			}
			sel := subscription.NewSelector(log, &fakeLister{subscriptions: []*unstructured.Unstructured{sub}}, nil, nil)

			//nolint:unqueryvet,nolintlint // False positive - not a SQL query
			_, err := sel.Select([]string{"g1"}, "", "", "ns/model-a")

			if tt.wantReason == "" {
				if err != nil {
					t.Fatalf("expected inference to be allowed, got %v", err)
				}
				return
			}
			var unhealthy *subscription.ModelUnhealthyError
			if !errors.As(err, &unhealthy) {
				t.Fatalf("expected ModelUnhealthyError, got %T: %v", err, err)
			}
			if unhealthy.Reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", unhealthy.Reason, tt.wantReason)
			}
		})
	}
}
