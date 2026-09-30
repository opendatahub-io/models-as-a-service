package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
)

// TestHandleSubscriptionSelectionError covers the mapping from selector errors to
// HTTP status. Subscription state that the caller caused — a subscription whose
// modelRefs do not resolve (phase Failed), or a model outside the subscription —
// must be a 4xx, not a 500. A Failed subscription reaching /v1/models previously
// fell through to "server_error".
func TestHandleSubscriptionSelectionError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantType    string
		wantMessage string
	}{
		{
			name:        "unhealthy model in failed subscription is not a server error",
			err:         &subscription.ModelUnhealthyError{Subscription: "simulator-subscription", Phase: "Failed", Reason: "NotFound"},
			wantStatus:  http.StatusForbidden,
			wantType:    "permission_error",
			wantMessage: "requested model is unhealthy in subscription",
		},
		{
			name:        "model not in subscription is unified with access denied",
			err:         &subscription.ModelNotInSubscriptionError{Subscription: "simulator-subscription", Model: "llm/other"},
			wantStatus:  http.StatusForbidden,
			wantType:    "permission_error",
			wantMessage: "access denied to requested subscription",
		},
		{
			name:        "access denied",
			err:         &subscription.AccessDeniedError{},
			wantStatus:  http.StatusForbidden,
			wantType:    "permission_error",
			wantMessage: "access denied to requested subscription",
		},
		{
			name:        "subscription not found is unified with access denied",
			err:         &subscription.SubscriptionNotFoundError{},
			wantStatus:  http.StatusForbidden,
			wantType:    "permission_error",
			wantMessage: "access denied to requested subscription",
		},
		{
			name:       "no subscription for user",
			err:        &subscription.NoSubscriptionError{},
			wantStatus: http.StatusForbidden,
			wantType:   "permission_error",
		},
		{
			name:       "unexpected error is still a server error",
			err:        errors.New("connection refused"),
			wantStatus: http.StatusInternalServerError,
			wantType:   "server_error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &ModelsHandler{logger: logger.Development()}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)

			h.handleSubscriptionSelectionError(c, tt.err)

			require.Equal(t, tt.wantStatus, w.Code)

			var body struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			assert.Equal(t, tt.wantType, body.Error.Type)
			if tt.wantMessage != "" {
				assert.Equal(t, tt.wantMessage, body.Error.Message)
			}
		})
	}
}

// TestModelUnhealthyErrorDoesNotLeakModelName guards the existing XSS note on
// ModelUnhealthyError: the response must not echo attacker-influenced model names.
func TestModelUnhealthyErrorDoesNotLeakModelName(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &ModelsHandler{logger: logger.Development()}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	h.handleSubscriptionSelectionError(c, &subscription.ModelUnhealthyError{
		Subscription: "<script>alert(1)</script>",
		Phase:        "Failed",
		Reason:       "NotFound",
		Message:      "<img src=x onerror=alert(1)>",
	})

	require.Equal(t, http.StatusForbidden, w.Code)
	assert.NotContains(t, w.Body.String(), "<script>")
	assert.NotContains(t, w.Body.String(), "onerror")
}
