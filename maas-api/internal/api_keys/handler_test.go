package api_keys //nolint:testpackage // Testing private helper methods requires same package

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/config"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

// Test constants.
const (
	testBulkRevokeAliceJSON = `{"username": "alice"}`
)

// mockAdminChecker is a simple mock for testing that checks if user has "admin-users" group.
type mockAdminChecker struct {
	adminGroups []string
}

func newMockAdminChecker() *mockAdminChecker {
	return &mockAdminChecker{
		adminGroups: []string{"admin-users"},
	}
}

func (m *mockAdminChecker) IsAdmin(userGroups []string) bool {
	for _, userGroup := range userGroups {
		if slices.Contains(m.adminGroups, userGroup) {
			return true
		}
	}
	return false
}

// executeSearchRequest is a test helper that executes a search request and returns the parsed response.
func executeSearchRequest(t *testing.T, handler *Handler, requestBody string, user *token.UserContext) SearchAPIKeysResponse {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/search", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
	c.Set("user", user)

	handler.SearchAPIKeys(c)

	assert.Equal(t, http.StatusOK, w.Code)
	var response SearchAPIKeysResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	return response
}

func TestIsAuthorizedForKey(t *testing.T) {
	h := &Handler{
		adminChecker: newMockAdminChecker(),
	}

	t.Run("OwnerCanAccess", func(t *testing.T) {
		user := &token.UserContext{Username: "alice", Groups: []string{"users"}}
		assert.True(t, h.isAuthorizedForKey(user, "alice"))
	})

	t.Run("NonOwnerCannotAccess", func(t *testing.T) {
		user := &token.UserContext{Username: "bob", Groups: []string{"users"}}
		assert.False(t, h.isAuthorizedForKey(user, "alice"))
	})

	t.Run("AdminCanAccessAnyKey", func(t *testing.T) {
		admin := &token.UserContext{Username: "admin", Groups: []string{"admin-users"}}
		assert.True(t, h.isAuthorizedForKey(admin, "alice"))
	})
}

// ============================================================
// SEARCH API TESTS (POST /v1/api-keys/search)
// ============================================================

func TestSearchAPIKeys_EmptyRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewMockStore()
	cfg := &config.Config{}
	service := NewServiceWithLogger(store, cfg, logger.Development())
	handler := NewHandler(logger.Development(), service, newMockAdminChecker())

	testUser := &token.UserContext{
		Username: "test-user",
		Groups:   []string{"system:authenticated"},
	}

	// Create test keys
	ctx := context.Background()
	err := store.AddKey(ctx, testUser.Username, "key-1", "hash-1", "Key 1", "", []string{"system:authenticated"}, nil)
	require.NoError(t, err)
	err = store.AddKey(ctx, testUser.Username, "key-2", "hash-2", "Key 2", "", []string{"system:authenticated"}, nil)
	require.NoError(t, err)
	// Create a revoked key
	err = store.AddKey(ctx, testUser.Username, "key-3", "hash-3", "Key 3", "", []string{"system:authenticated"}, nil)
	require.NoError(t, err)
	err = store.Revoke(ctx, "key-3")
	require.NoError(t, err)

	// Empty request should use defaults: all statuses, created_at desc, limit 50
	requestBody := `{}`

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/search", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
	c.Set("user", testUser)

	handler.SearchAPIKeys(c)

	assert.Equal(t, http.StatusOK, w.Code)
	var response SearchAPIKeysResponse
	err = json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)

	assert.Equal(t, "list", response.Object)
	assert.Len(t, response.Data, 3, "should return all keys (all statuses) by default")
	assert.False(t, response.HasMore)
}

func TestSearchAPIKeys_Pagination(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewMockStore()
	cfg := &config.Config{}
	service := NewServiceWithLogger(store, cfg, logger.Development())
	handler := NewHandler(logger.Development(), service, newMockAdminChecker())

	testUser := &token.UserContext{
		Username: "test-user",
		Groups:   []string{"system:authenticated"},
	}

	// Add 75 keys to test pagination
	ctx := context.Background()
	for i := 1; i <= 75; i++ {
		keyID := fmt.Sprintf("key-%d", i)
		keyHash := fmt.Sprintf("hash-%d", i)
		name := fmt.Sprintf("Key %d", i)
		err := store.AddKey(ctx, testUser.Username, keyID, keyHash, name, "", []string{"system:authenticated"}, nil)
		require.NoError(t, err)
	}

	t.Run("DefaultPagination", func(t *testing.T) {
		requestBody := `{}`

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/search", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", testUser)

		handler.SearchAPIKeys(c)

		assert.Equal(t, http.StatusOK, w.Code)
		var response SearchAPIKeysResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.Equal(t, "list", response.Object)
		assert.Len(t, response.Data, 50, "should use default limit of 50")
		assert.True(t, response.HasMore, "should indicate more pages exist")
	})

	t.Run("CustomPagination", func(t *testing.T) {
		requestBody := `{"pagination": {"limit": 10, "offset": 0}}`

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/search", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", testUser)

		handler.SearchAPIKeys(c)

		assert.Equal(t, http.StatusOK, w.Code)
		var response SearchAPIKeysResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		assert.Len(t, response.Data, 10)
		assert.True(t, response.HasMore)
	})

	t.Run("InvalidLimit", func(t *testing.T) {
		requestBody := `{"pagination": {"limit": 0}}`

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/search", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", testUser)

		handler.SearchAPIKeys(c)

		assert.Equal(t, http.StatusBadRequest, w.Code)
		var response map[string]string
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Contains(t, response["error"], "limit must be at least 1")
	})

	t.Run("NegativeOffset", func(t *testing.T) {
		requestBody := `{"pagination": {"limit": 10, "offset": -1}}`

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/search", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", testUser)

		handler.SearchAPIKeys(c)

		assert.Equal(t, http.StatusBadRequest, w.Code)
		var response map[string]string
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Contains(t, response["error"], "offset must be non-negative")
	})
}

func TestSearchAPIKeys_StatusFilter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewMockStore()
	cfg := &config.Config{}
	service := NewServiceWithLogger(store, cfg, logger.Development())
	handler := NewHandler(logger.Development(), service, newMockAdminChecker())

	ctx := context.Background()
	testUser := &token.UserContext{
		Username: "test-user",
		Groups:   []string{"system:authenticated"},
	}

	// Create active and revoked keys
	err := store.AddKey(ctx, testUser.Username, "active-key", "active-hash", "Active Key", "", []string{"system:authenticated"}, nil)
	require.NoError(t, err)
	err = store.AddKey(ctx, testUser.Username, "revoked-key", "revoked-hash", "Revoked Key", "", []string{"system:authenticated"}, nil)
	require.NoError(t, err)
	err = store.Revoke(ctx, "revoked-key")
	require.NoError(t, err)

	t.Run("DefaultAllStatuses", func(t *testing.T) {
		requestBody := `{}`

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/search", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", testUser)

		handler.SearchAPIKeys(c)

		assert.Equal(t, http.StatusOK, w.Code)
		var response SearchAPIKeysResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Len(t, response.Data, 2, "should return all keys (all statuses) by default")
		// Verify we got both active and revoked keys
		statuses := make(map[Status]int)
		for _, key := range response.Data {
			statuses[key.Status]++
		}
		assert.Equal(t, 1, statuses[StatusActive], "should have 1 active key")
		assert.Equal(t, 1, statuses[StatusRevoked], "should have 1 revoked key")
	})

	t.Run("ExplicitActiveFilter", func(t *testing.T) {
		requestBody := `{"filters": {"status": ["active"]}}`

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/search", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", testUser)

		handler.SearchAPIKeys(c)

		assert.Equal(t, http.StatusOK, w.Code)
		var response SearchAPIKeysResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Len(t, response.Data, 1)
		assert.Equal(t, StatusActive, response.Data[0].Status)
	})

	t.Run("RevokedFilter", func(t *testing.T) {
		requestBody := `{"filters": {"status": ["revoked"]}}`

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/search", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", testUser)

		handler.SearchAPIKeys(c)

		assert.Equal(t, http.StatusOK, w.Code)
		var response SearchAPIKeysResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Len(t, response.Data, 1)
		assert.Equal(t, StatusRevoked, response.Data[0].Status)
	})

	t.Run("AllStatuses", func(t *testing.T) {
		requestBody := `{"filters": {"status": ["active", "revoked", "expired"]}}`

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/search", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", testUser)

		handler.SearchAPIKeys(c)

		assert.Equal(t, http.StatusOK, w.Code)
		var response SearchAPIKeysResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Len(t, response.Data, 2, "should return all keys")
	})

	t.Run("InvalidStatusReturnsError", func(t *testing.T) {
		requestBody := `{"filters": {"status": ["invalid"]}}`

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/search", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", testUser)

		handler.SearchAPIKeys(c)

		assert.Equal(t, http.StatusBadRequest, w.Code)
		var response map[string]string
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Contains(t, response["error"], "invalid status")
	})
}

func TestSearchAPIKeys_Sorting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewMockStore()
	cfg := &config.Config{}
	service := NewServiceWithLogger(store, cfg, logger.Development())
	handler := NewHandler(logger.Development(), service, newMockAdminChecker())

	ctx := context.Background()
	testUser := &token.UserContext{
		Username: "test-user",
		Groups:   []string{"system:authenticated"},
	}

	// Create keys with different names
	err := store.AddKey(ctx, testUser.Username, "key-1", "hash-1", "Charlie", "", []string{"system:authenticated"}, nil)
	require.NoError(t, err)
	err = store.AddKey(ctx, testUser.Username, "key-2", "hash-2", "Alice", "", []string{"system:authenticated"}, nil)
	require.NoError(t, err)
	err = store.AddKey(ctx, testUser.Username, "key-3", "hash-3", "Bob", "", []string{"system:authenticated"}, nil)
	require.NoError(t, err)

	t.Run("DefaultSort_CreatedAtDesc", func(t *testing.T) {
		requestBody := `{}`

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/search", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", testUser)

		handler.SearchAPIKeys(c)

		assert.Equal(t, http.StatusOK, w.Code)
		var response SearchAPIKeysResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)

		// Should be sorted by created_at desc (all 3 keys present)
		assert.Len(t, response.Data, 3)
		// Don't check exact order as timing may vary, just verify all keys are returned
		names := []string{response.Data[0].Name, response.Data[1].Name, response.Data[2].Name}
		assert.Contains(t, names, "Alice")
		assert.Contains(t, names, "Bob")
		assert.Contains(t, names, "Charlie")
	})

	t.Run("SortByNameAsc", func(t *testing.T) {
		requestBody := `{"sort": {"by": "name", "order": "asc"}}`
		response := executeSearchRequest(t, handler, requestBody, testUser)

		assert.Len(t, response.Data, 3)
		assert.Equal(t, "Alice", response.Data[0].Name)
		assert.Equal(t, "Bob", response.Data[1].Name)
		assert.Equal(t, "Charlie", response.Data[2].Name)
	})

	t.Run("SortByNameDesc", func(t *testing.T) {
		requestBody := `{"sort": {"by": "name", "order": "desc"}}`
		response := executeSearchRequest(t, handler, requestBody, testUser)

		assert.Len(t, response.Data, 3)
		assert.Equal(t, "Charlie", response.Data[0].Name)
		assert.Equal(t, "Bob", response.Data[1].Name)
		assert.Equal(t, "Alice", response.Data[2].Name)
	})

	t.Run("CaseInsensitiveOrder", func(t *testing.T) {
		requestBody := `{"sort": {"by": "name", "order": "ASC"}}`

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/search", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", testUser)

		handler.SearchAPIKeys(c)

		assert.Equal(t, http.StatusOK, w.Code)
		var response SearchAPIKeysResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Len(t, response.Data, 3)
	})

	t.Run("InvalidSortField", func(t *testing.T) {
		requestBody := `{"sort": {"by": "invalid_field"}}`

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/search", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", testUser)

		handler.SearchAPIKeys(c)

		assert.Equal(t, http.StatusBadRequest, w.Code)
		var response map[string]string
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Contains(t, response["error"], "invalid sort.by")
	})

	t.Run("InvalidSortOrder", func(t *testing.T) {
		requestBody := `{"sort": {"order": "invalid"}}`

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/search", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", testUser)

		handler.SearchAPIKeys(c)

		assert.Equal(t, http.StatusBadRequest, w.Code)
		var response map[string]string
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Contains(t, response["error"], "invalid sort.order")
	})
}

func TestSearchAPIKeys_AdminVsRegularUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewMockStore()
	cfg := &config.Config{}
	service := NewServiceWithLogger(store, cfg, logger.Development())
	handler := NewHandler(logger.Development(), service, newMockAdminChecker())

	ctx := context.Background()

	// Create keys for multiple users
	users := []string{"alice", "bob"}
	for _, username := range users {
		for i := 1; i <= 2; i++ {
			keyID := fmt.Sprintf("%s-key-%d", username, i)
			keyHash := fmt.Sprintf("%s-hash-%d", username, i)
			name := fmt.Sprintf("%s Key %d", username, i)
			err := store.AddKey(ctx, username, keyID, keyHash, name, "", []string{"system:authenticated"}, nil)
			require.NoError(t, err)
		}
	}

	t.Run("AdminSeesAllKeys", func(t *testing.T) {
		adminUser := &token.UserContext{
			Username: "admin",
			Groups:   []string{"admin-users"},
		}

		requestBody := `{}`

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/search", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", adminUser)

		handler.SearchAPIKeys(c)

		assert.Equal(t, http.StatusOK, w.Code)
		var response SearchAPIKeysResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Len(t, response.Data, 4, "admin should see all 4 keys")
	})

	t.Run("RegularUserOnlySeesOwnKeys", func(t *testing.T) {
		regularUser := &token.UserContext{
			Username: "alice",
			Groups:   []string{"system:authenticated"},
		}

		requestBody := `{}`

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/search", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", regularUser)

		handler.SearchAPIKeys(c)

		assert.Equal(t, http.StatusOK, w.Code)
		var response SearchAPIKeysResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Len(t, response.Data, 2, "regular user should only see own keys")
		for _, key := range response.Data {
			assert.Equal(t, "alice", key.Username)
		}
	})

	t.Run("RegularUserCannotFilterOtherUser", func(t *testing.T) {
		regularUser := &token.UserContext{
			Username: "alice",
			Groups:   []string{"system:authenticated"},
		}

		requestBody := `{"filters": {"username": "bob"}}`

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/search", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", regularUser)

		handler.SearchAPIKeys(c)

		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("AdminCanFilterByUsername", func(t *testing.T) {
		adminUser := &token.UserContext{
			Username: "admin",
			Groups:   []string{"admin-users"},
		}

		requestBody := `{"filters": {"username": "alice"}}`

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/search", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", adminUser)

		handler.SearchAPIKeys(c)

		assert.Equal(t, http.StatusOK, w.Code)
		var response SearchAPIKeysResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Len(t, response.Data, 2)
		for _, key := range response.Data {
			assert.Equal(t, "alice", key.Username)
		}
	})
}

func TestSearchAPIKeys_AdminFiltersByUsernameAndStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewMockStore()
	cfg := &config.Config{}
	service := NewServiceWithLogger(store, cfg, logger.Development())
	handler := NewHandler(logger.Development(), service, newMockAdminChecker())

	ctx := context.Background()

	// Create 6 keys total: alice (2 active, 1 revoked), bob (2 active, 1 revoked)
	users := []string{"alice", "bob"}
	for _, username := range users {
		// Create 2 active keys
		for i := 1; i <= 2; i++ {
			keyID := fmt.Sprintf("%s-active-%d", username, i)
			keyHash := fmt.Sprintf("%s-hash-active-%d", username, i)
			name := fmt.Sprintf("%s Active Key %d", username, i)
			err := store.AddKey(ctx, username, keyID, keyHash, name, "", []string{"system:authenticated"}, nil)
			require.NoError(t, err)
		}
		// Create 1 revoked key
		keyID := fmt.Sprintf("%s-revoked", username)
		keyHash := fmt.Sprintf("%s-hash-revoked", username)
		name := fmt.Sprintf("%s Revoked Key", username)
		err := store.AddKey(ctx, username, keyID, keyHash, name, "", []string{"system:authenticated"}, nil)
		require.NoError(t, err)
		err = store.Revoke(ctx, keyID)
		require.NoError(t, err)
	}

	adminUser := &token.UserContext{
		Username: "admin",
		Groups:   []string{"admin-users"},
	}

	t.Run("AliceActiveKeys", func(t *testing.T) {
		requestBody := `{"filters": {"username": "alice", "status": ["active"]}}`
		response := executeSearchRequest(t, handler, requestBody, adminUser)

		assert.Len(t, response.Data, 2, "alice should have 2 active keys")
		for _, key := range response.Data {
			assert.Equal(t, "alice", key.Username)
			assert.Equal(t, StatusActive, key.Status)
		}
	})

	t.Run("AliceRevokedKeys", func(t *testing.T) {
		requestBody := `{"filters": {"username": "alice", "status": ["revoked"]}}`

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/search", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", adminUser)

		handler.SearchAPIKeys(c)

		assert.Equal(t, http.StatusOK, w.Code)
		var response SearchAPIKeysResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Len(t, response.Data, 1, "alice should have 1 revoked key")
		assert.Equal(t, "alice", response.Data[0].Username)
		assert.Equal(t, StatusRevoked, response.Data[0].Status)
	})

	t.Run("BobActiveKeys", func(t *testing.T) {
		requestBody := `{"filters": {"username": "bob", "status": ["active"]}}`
		response := executeSearchRequest(t, handler, requestBody, adminUser)

		assert.Len(t, response.Data, 2, "bob should have 2 active keys")
		for _, key := range response.Data {
			assert.Equal(t, "bob", key.Username)
			assert.Equal(t, StatusActive, key.Status)
		}
	})
}

// ============================================================
// BULK REVOKE TESTS (POST /v1/api-keys/bulk-revoke)
// ============================================================

func TestBulkRevokeAPIKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewMockStore()
	cfg := &config.Config{}
	service := NewServiceWithLogger(store, cfg, logger.Development())
	handler := NewHandler(logger.Development(), service, newMockAdminChecker())

	ctx := context.Background()

	// Create keys for alice and bob
	for i := 1; i <= 3; i++ {
		keyID := fmt.Sprintf("alice-key-%d", i)
		keyHash := fmt.Sprintf("alice-hash-%d", i)
		name := fmt.Sprintf("Alice Key %d", i)
		err := store.AddKey(ctx, "alice", keyID, keyHash, name, "", []string{"system:authenticated"}, nil)
		require.NoError(t, err)
	}

	for i := 1; i <= 2; i++ {
		keyID := fmt.Sprintf("bob-key-%d", i)
		keyHash := fmt.Sprintf("bob-hash-%d", i)
		name := fmt.Sprintf("Bob Key %d", i)
		err := store.AddKey(ctx, "bob", keyID, keyHash, name, "", []string{"system:authenticated"}, nil)
		require.NoError(t, err)
	}

	t.Run("UserCanRevokeOwnKeys", func(t *testing.T) {
		aliceUser := &token.UserContext{
			Username: "alice",
			Groups:   []string{"system:authenticated"},
		}

		requestBody := testBulkRevokeAliceJSON

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/bulk-revoke", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", aliceUser)

		handler.BulkRevokeAPIKeys(c)

		assert.Equal(t, http.StatusOK, w.Code)
		var response BulkRevokeResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Equal(t, 3, response.RevokedCount)
		assert.Contains(t, response.Message, "Successfully revoked 3")
	})

	t.Run("UserCannotRevokeOtherKeys", func(t *testing.T) {
		bobUser := &token.UserContext{
			Username: "bob",
			Groups:   []string{"system:authenticated"},
		}

		requestBody := testBulkRevokeAliceJSON

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/bulk-revoke", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", bobUser)

		handler.BulkRevokeAPIKeys(c)

		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("AdminCanRevokeAnyUserKeys", func(t *testing.T) {
		// Re-add alice keys (they were revoked in first test)
		for i := 4; i <= 6; i++ {
			keyID := fmt.Sprintf("alice-key-%d", i)
			keyHash := fmt.Sprintf("alice-hash-%d", i)
			name := fmt.Sprintf("Alice Key %d", i)
			err := store.AddKey(ctx, "alice", keyID, keyHash, name, "", []string{"system:authenticated"}, nil)
			require.NoError(t, err)
		}

		adminUser := &token.UserContext{
			Username: "admin",
			Groups:   []string{"admin-users"},
		}

		requestBody := testBulkRevokeAliceJSON

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/bulk-revoke", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", adminUser)

		//nolint:contextcheck // Gin handlers receive *gin.Context which contains the context.
		handler.BulkRevokeAPIKeys(c)

		assert.Equal(t, http.StatusOK, w.Code)
		var response BulkRevokeResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Equal(t, 3, response.RevokedCount)
	})

	t.Run("RevokeAlreadyRevokedReturnsZero", func(t *testing.T) {
		aliceUser := &token.UserContext{
			Username: "alice",
			Groups:   []string{"system:authenticated"},
		}

		requestBody := testBulkRevokeAliceJSON

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/bulk-revoke", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", aliceUser)

		handler.BulkRevokeAPIKeys(c)

		assert.Equal(t, http.StatusOK, w.Code)
		var response BulkRevokeResponse
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Equal(t, 0, response.RevokedCount, "no active keys to revoke")
	})

	t.Run("MissingUsernameReturnsError", func(t *testing.T) {
		aliceUser := &token.UserContext{
			Username: "alice",
			Groups:   []string{"system:authenticated"},
		}

		requestBody := `{}`

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys/bulk-revoke", nil)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
		c.Set("user", aliceUser)

		handler.BulkRevokeAPIKeys(c)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

// ============================================================
// CREATE API KEY TESTS
// ============================================================

func TestUserCanCreateOwnKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewMockStore()
	cfg := &config.Config{}
	service := NewServiceWithLogger(store, cfg, logger.Development())
	handler := NewHandler(logger.Development(), service, newMockAdminChecker())

	regularUser := &token.UserContext{
		Username: "alice",
		Groups:   []string{"tier-free", "system:authenticated"},
	}

	requestBody := `{"name": "my-key"}`

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Body = io.NopCloser(strings.NewReader(requestBody))
	c.Set("user", regularUser)

	handler.CreateAPIKey(c)

	assert.Equal(t, http.StatusCreated, w.Code)
	var response CreateAPIKeyResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)

	// Verify key is owned by alice with her actual groups
	meta, err := store.Get(context.Background(), response.ID)
	require.NoError(t, err)
	assert.Equal(t, "alice", meta.Username)
	assert.Equal(t, []string{"tier-free", "system:authenticated"}, meta.Groups)
}

// ============================================================
// GET API KEY HANDLER TESTS (GET /v1/api-keys/:id)
// ============================================================

func TestGetAPIKeyHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewMockStore()
	cfg := &config.Config{}
	service := NewServiceWithLogger(store, cfg, logger.Development())
	handler := NewHandler(logger.Development(), service, newMockAdminChecker())

	// Create test keys for alice and bob
	aliceKey := &ApiKey{
		ID:           "alice-key-1",
		Username:     "alice",
		Groups:       []string{"tier-free"},
		Name:         "Alice's Key",
		Status:       StatusActive,
		CreationDate: "2025-01-15T10:00:00Z",
	}
	bobKey := &ApiKey{
		ID:           "bob-key-1",
		Username:     "bob",
		Groups:       []string{"tier-premium"},
		Name:         "Bob's Key",
		Status:       StatusActive,
		CreationDate: "2025-01-16T10:00:00Z",
	}

	// Add keys to store
	err := store.AddKey(context.Background(), aliceKey.Username, aliceKey.ID, "hash1", aliceKey.Name, "", aliceKey.Groups, nil)
	require.NoError(t, err)
	err = store.AddKey(context.Background(), bobKey.Username, bobKey.ID, "hash2", bobKey.Name, "", bobKey.Groups, nil)
	require.NoError(t, err)

	t.Run("OwnerCanGetOwnKey", func(t *testing.T) {
		aliceUser := &token.UserContext{
			Username: "alice",
			Groups:   []string{"tier-free"},
		}

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/api-keys/alice-key-1", nil)
		c.Set("user", aliceUser)
		c.Params = gin.Params{{Key: "id", Value: "alice-key-1"}}

		handler.GetAPIKey(c)

		assert.Equal(t, http.StatusOK, w.Code)
		var response ApiKey
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Equal(t, "alice-key-1", response.ID)
		assert.Equal(t, "alice", response.Username)
	})

	t.Run("RegularUserCannotGetOthersKey_IDOR_Protection", func(t *testing.T) {
		// Bob trying to access Alice's key
		bobUser := &token.UserContext{
			Username: "bob",
			Groups:   []string{"tier-premium"},
		}

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/api-keys/alice-key-1", nil)
		c.Set("user", bobUser)
		c.Params = gin.Params{{Key: "id", Value: "alice-key-1"}}

		handler.GetAPIKey(c)

		// IDOR Protection: Return 404 instead of 403 to prevent key enumeration
		assert.Equal(t, http.StatusNotFound, w.Code)
		var response map[string]string
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Equal(t, "API key not found", response["error"])
	})

	t.Run("AdminCanGetAnyKey", func(t *testing.T) {
		adminUser := &token.UserContext{
			Username: "admin",
			Groups:   []string{"admin-users"},
		}

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/api-keys/alice-key-1", nil)
		c.Set("user", adminUser)
		c.Params = gin.Params{{Key: "id", Value: "alice-key-1"}}

		handler.GetAPIKey(c)

		assert.Equal(t, http.StatusOK, w.Code)
		var response ApiKey
		err := json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Equal(t, "alice-key-1", response.ID)
		assert.Equal(t, "alice", response.Username)
	})

	t.Run("NonExistentKeyReturns404", func(t *testing.T) {
		aliceUser := &token.UserContext{
			Username: "alice",
			Groups:   []string{"tier-free"},
		}

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/api-keys/non-existent", nil)
		c.Set("user", aliceUser)
		c.Params = gin.Params{{Key: "id", Value: "non-existent"}}

		handler.GetAPIKey(c)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

// ============================================================
// REVOKE API KEY HANDLER TESTS (DELETE /v1/api-keys/:id)
// ============================================================

// testRevokeKeySuccess is a helper to test successful key revocation.
func testRevokeKeySuccess(t *testing.T, user *token.UserContext) {
	t.Helper()
	store := NewMockStore()
	cfg := &config.Config{}
	service := NewServiceWithLogger(store, cfg, logger.Development())
	handler := NewHandler(logger.Development(), service, newMockAdminChecker())

	// Create alice's key
	err := store.AddKey(context.Background(), "alice", "alice-key-1", "hash1", "Alice's Key", "", []string{"tier-free"}, nil)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/v1/api-keys/alice-key-1", nil)
	c.Set("user", user)
	c.Params = gin.Params{{Key: "id", Value: "alice-key-1"}}

	handler.RevokeAPIKey(c)

	// Per OpenAPI spec: returns 200 with revoked key metadata
	assert.Equal(t, http.StatusOK, w.Code)
	var response ApiKey
	err = json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Equal(t, "alice-key-1", response.ID)
	assert.Equal(t, StatusRevoked, response.Status)

	// Verify key is revoked in store
	key, err := store.Get(context.Background(), "alice-key-1")
	require.NoError(t, err)
	assert.Equal(t, StatusRevoked, key.Status)
}

func TestRevokeAPIKeyHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("OwnerCanRevokeOwnKey", func(t *testing.T) {
		aliceUser := &token.UserContext{
			Username: "alice",
			Groups:   []string{"tier-free"},
		}
		testRevokeKeySuccess(t, aliceUser)
	})

	t.Run("RegularUserCannotRevokeOthersKey_IDOR_Protection", func(t *testing.T) {
		store := NewMockStore()
		cfg := &config.Config{}
		service := NewServiceWithLogger(store, cfg, logger.Development())
		handler := NewHandler(logger.Development(), service, newMockAdminChecker())

		// Create alice's key
		err := store.AddKey(context.Background(), "alice", "alice-key-1", "hash1", "Alice's Key", "", []string{"tier-free"}, nil)
		require.NoError(t, err)

		// Bob trying to revoke Alice's key
		bobUser := &token.UserContext{
			Username: "bob",
			Groups:   []string{"tier-premium"},
		}

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodDelete, "/v1/api-keys/alice-key-1", nil)
		c.Set("user", bobUser)
		c.Params = gin.Params{{Key: "id", Value: "alice-key-1"}}

		handler.RevokeAPIKey(c)

		// IDOR Protection: Return 404 instead of 403 to prevent key enumeration
		assert.Equal(t, http.StatusNotFound, w.Code)
		var response map[string]string
		err = json.Unmarshal(w.Body.Bytes(), &response)
		require.NoError(t, err)
		assert.Equal(t, "API key not found", response["error"])

		// Verify key was NOT revoked
		key, err := store.Get(context.Background(), "alice-key-1")
		require.NoError(t, err)
		assert.Equal(t, StatusActive, key.Status)
	})

	t.Run("AdminCanRevokeAnyKey", func(t *testing.T) {
		adminUser := &token.UserContext{
			Username: "admin",
			Groups:   []string{"admin-users"},
		}
		testRevokeKeySuccess(t, adminUser)
	})

	t.Run("NonExistentKeyReturns404", func(t *testing.T) {
		store := NewMockStore()
		cfg := &config.Config{}
		service := NewServiceWithLogger(store, cfg, logger.Development())
		handler := NewHandler(logger.Development(), service, newMockAdminChecker())

		aliceUser := &token.UserContext{
			Username: "alice",
			Groups:   []string{"tier-free"},
		}

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodDelete, "/v1/api-keys/non-existent", nil)
		c.Set("user", aliceUser)
		c.Params = gin.Params{{Key: "id", Value: "non-existent"}}

		handler.RevokeAPIKey(c)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("CannotRevokeAlreadyRevokedKey", func(t *testing.T) {
		store := NewMockStore()
		cfg := &config.Config{}
		service := NewServiceWithLogger(store, cfg, logger.Development())
		handler := NewHandler(logger.Development(), service, newMockAdminChecker())

		// Create and immediately revoke alice's key
		err := store.AddKey(context.Background(), "alice", "alice-key-1", "hash1", "Alice's Key", "", []string{"tier-free"}, nil)
		require.NoError(t, err)
		err = store.Revoke(context.Background(), "alice-key-1")
		require.NoError(t, err)

		aliceUser := &token.UserContext{
			Username: "alice",
			Groups:   []string{"tier-free"},
		}

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodDelete, "/v1/api-keys/alice-key-1", nil)
		c.Set("user", aliceUser)
		c.Params = gin.Params{{Key: "id", Value: "alice-key-1"}}

		handler.RevokeAPIKey(c)

		// Already revoked key returns 404 (not found among active keys)
		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}
