package api_keys //nolint:testpackage // Testing private helper methods requires same package

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/config"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

func TestIsAuthorizedForKey(t *testing.T) {
	tests := []struct {
		name     string
		user     *token.UserContext
		keyOwner string
		want     bool
	}{
		{
			name: "owner can access their own key",
			user: &token.UserContext{
				Username: "alice",
				Groups:   []string{"users"},
			},
			keyOwner: "alice",
			want:     true,
		},
		{
			name: "non-owner cannot access key",
			user: &token.UserContext{
				Username: "bob",
				Groups:   []string{"users"},
			},
			keyOwner: "alice",
			want:     false,
		},
		{
			name: "admin can access any key",
			user: &token.UserContext{
				Username: "admin-user",
				Groups:   []string{"admin-users", "users"},
			},
			keyOwner: "alice",
			want:     true,
		},
		{
			name: "admin with different username can access key",
			user: &token.UserContext{
				Username: "super-admin",
				Groups:   []string{"admin-users"},
			},
			keyOwner: "alice",
			want:     true,
		},
		{
			name: "user with no groups cannot access other's key",
			user: &token.UserContext{
				Username: "charlie",
				Groups:   []string{},
			},
			keyOwner: "alice",
			want:     false,
		},
		{
			name: "user with different groups cannot access other's key",
			user: &token.UserContext{
				Username: "dave",
				Groups:   []string{"developers", "qa-team"},
			},
			keyOwner: "alice",
			want:     false,
		},
		{
			name: "case-sensitive username match required",
			user: &token.UserContext{
				Username: "Alice",
				Groups:   []string{"users"},
			},
			keyOwner: "alice",
			want:     false,
		},
		{
			name: "empty username cannot access key",
			user: &token.UserContext{
				Username: "",
				Groups:   []string{"users"},
			},
			keyOwner: "alice",
			want:     false,
		},
		{
			name: "admin group check is case-sensitive",
			user: &token.UserContext{
				Username: "admin",
				Groups:   []string{"Admin-Users"}, // Wrong case
			},
			keyOwner: "alice",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Handler{}
			got := h.isAuthorizedForKey(tt.user, tt.keyOwner)
			if got != tt.want {
				t.Errorf("isAuthorizedForKey() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestListAPIKeysPagination(t *testing.T) {
	// Setup
	gin.SetMode(gin.TestMode)
	store := NewMockStore()
	cfg := &config.Config{}
	service := NewServiceWithLogger(nil, store, cfg, logger.Development())
	handler := NewHandler(logger.Development(), service)

	// Create test user and keys
	testUser := &token.UserContext{
		Username: "test-user",
		Groups:   []string{"system:authenticated"},
	}

	// Add 75 keys to test pagination
	for i := 1; i <= 75; i++ {
		keyID := fmt.Sprintf("key-%d", i)
		keyHash := fmt.Sprintf("hash-%d", i)
		keyPrefix := fmt.Sprintf("sk-oai-%03d", i)
		name := fmt.Sprintf("Key %d", i)
		err := store.AddKey(context.Background(), testUser.Username, keyID, keyHash, keyPrefix, name, "", `["system:authenticated"]`, nil)
		require.NoError(t, err)
	}

	t.Run("DefaultPagination", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/v2/api-keys", nil)
		c.Set("user", testUser)

		handler.ListAPIKeys(c)

		assert.Equal(t, http.StatusOK, w.Code)

		var response ListAPIKeysResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.Equal(t, "list", response.Object)
		assert.Len(t, response.Data, 50, "should use default limit of 50")
		assert.True(t, response.HasMore, "should indicate more pages exist")
	})

	t.Run("InvalidLimit", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/v2/api-keys?limit=abc", nil)
		c.Set("user", testUser)

		handler.ListAPIKeys(c)

		assert.Equal(t, http.StatusBadRequest, w.Code)

		var response map[string]string
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Contains(t, response["error"], "invalid limit parameter")
	})
}
