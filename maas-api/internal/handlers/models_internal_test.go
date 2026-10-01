package handlers

import (
	"testing"

	"github.com/openai/openai-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/models"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
)

func TestFilterModelsBySubscription(t *testing.T) {
	// OwnedBy carries the MaaSModelRef namespace/name, which is what modelRefs match on.
	modelList := []models.Model{
		{Model: openai.Model{ID: "free-model", OwnedBy: "llm/free-model"}, Ready: true},
		{Model: openai.Model{ID: "premium-model", OwnedBy: "llm/premium-model"}, Ready: true},
	}

	tests := []struct {
		name      string
		modelRefs []subscription.ModelRefInfo
		wantIDs   []string
	}{
		{
			name:      "keeps only models the subscription references",
			modelRefs: []subscription.ModelRefInfo{{Name: "free-model", Namespace: "llm"}},
			wantIDs:   []string{"free-model"},
		},
		{
			name:      "same name in another namespace is not a match",
			modelRefs: []subscription.ModelRefInfo{{Name: "free-model", Namespace: "other"}},
			wantIDs:   []string{},
		},
		{
			name:      "nil modelRefs grant no models",
			modelRefs: nil,
			wantIDs:   []string{},
		},
		{
			name:      "empty modelRefs grant no models",
			modelRefs: []subscription.ModelRefInfo{},
			wantIDs:   []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filtered := filterModelsBySubscription(modelList, tt.modelRefs)

			// Non-nil so the listing marshals as [] rather than null.
			require.NotNil(t, filtered)
			ids := make([]string, 0, len(filtered))
			for _, m := range filtered {
				ids = append(ids, m.ID)
			}
			assert.Equal(t, tt.wantIDs, ids)
		})
	}
}
