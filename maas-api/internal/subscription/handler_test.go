package subscription_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/subscription"
)

// mockLister implements subscription.Lister for testing.
type mockLister struct {
	subscriptions []*unstructured.Unstructured
}

func (m *mockLister) List() ([]*unstructured.Unstructured, error) {
	return m.subscriptions, nil
}

func createTestSubscription(name string, groups []string, priority int32, orgID, costCenter string) *unstructured.Unstructured {
	groupsSlice := make([]any, len(groups))
	for i, g := range groups {
		groupsSlice[i] = map[string]any{"name": g}
	}

	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "maas.opendatahub.io/v1alpha1",
			"kind":       "MaaSSubscription",
			"metadata": map[string]any{
				"name":      name,
				"namespace": "test-ns",
			},
			"spec": map[string]any{
				"owner": map[string]any{
					"groups": groupsSlice,
				},
				"priority": int64(priority),
				"modelRefs": []any{
					map[string]any{
						"name": "test-model",
						"tokenRateLimits": []any{
							map[string]any{
								"limit": int64(1000),
							},
						},
					},
				},
				"tokenMetadata": map[string]any{
					"organizationId": orgID,
					"costCenter":     costCenter,
					"labels": map[string]any{
						"env": "test",
					},
				},
			},
		},
	}
}

func setupTestRouter(lister subscription.Lister) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	log := logger.New(false)
	selector := subscription.NewSelector(log, lister)
	handler := subscription.NewHandler(log, selector)

	router.POST("/subscriptions/select", handler.SelectSubscription)
	return router
}

func TestHandler_SelectSubscription_Success(t *testing.T) {
	subscriptions := []*unstructured.Unstructured{
		createTestSubscription("basic-sub", []string{"basic-users"}, 10, "org-basic", "cc-basic"),
		createTestSubscription("premium-sub", []string{"premium-users"}, 20, "org-premium", "cc-premium"),
	}

	lister := &mockLister{subscriptions: subscriptions}
	router := setupTestRouter(lister)

	tests := []struct {
		name                  string
		groups                []string
		username              string
		requestedSubscription string
		expectedName          string
		expectedOrgID         string
		expectedCode          int
	}{
		{
			name:          "auto-select premium subscription",
			groups:        []string{"premium-users"},
			username:      "alice",
			expectedName:  "premium-sub",
			expectedOrgID: "org-premium",
			expectedCode:  http.StatusOK,
		},
		{
			name:          "auto-select basic subscription",
			groups:        []string{"basic-users"},
			username:      "bob",
			expectedName:  "basic-sub",
			expectedOrgID: "org-basic",
			expectedCode:  http.StatusOK,
		},
		{
			name:                  "explicit selection with access",
			groups:                []string{"basic-users", "premium-users"},
			username:              "charlie",
			requestedSubscription: "basic-sub",
			expectedName:          "basic-sub",
			expectedOrgID:         "org-basic",
			expectedCode:          http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqBody := subscription.SelectRequest{
				Groups:                tt.groups,
				Username:              tt.username,
				RequestedSubscription: tt.requestedSubscription,
			}
			jsonBody, err := json.Marshal(reqBody)
			if err != nil {
				t.Fatalf("failed to marshal request: %v", err)
			}

			req := httptest.NewRequest(http.MethodPost, "/subscriptions/select", bytes.NewBuffer(jsonBody))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			router.ServeHTTP(w, req)

			if w.Code != tt.expectedCode {
				t.Errorf("expected status %d, got %d", tt.expectedCode, w.Code)
			}

			if w.Code == http.StatusOK {
				var response subscription.SelectResponse
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
					t.Fatalf("failed to unmarshal response: %v", err)
				}

				if response.Name != tt.expectedName {
					t.Errorf("expected subscription %q, got %q", tt.expectedName, response.Name)
				}

				if response.OrganizationID != tt.expectedOrgID {
					t.Errorf("expected orgID %q, got %q", tt.expectedOrgID, response.OrganizationID)
				}
			}
		})
	}
}

func TestHandler_SelectSubscription_NotFound(t *testing.T) {
	subscriptions := []*unstructured.Unstructured{
		createTestSubscription("premium-sub", []string{"premium-users"}, 20, "org-premium", "cc-premium"),
	}

	lister := &mockLister{subscriptions: subscriptions}
	router := setupTestRouter(lister)

	reqBody := subscription.SelectRequest{
		Groups:   []string{"other-group"},
		Username: "alice",
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/subscriptions/select", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var response subscription.SelectResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if response.Error != "not_found" {
		t.Errorf("expected error code 'not_found', got %q", response.Error)
	}
}

func TestHandler_SelectSubscription_AccessDenied(t *testing.T) {
	subscriptions := []*unstructured.Unstructured{
		createTestSubscription("premium-sub", []string{"premium-users"}, 20, "org-premium", "cc-premium"),
	}

	lister := &mockLister{subscriptions: subscriptions}
	router := setupTestRouter(lister)

	reqBody := subscription.SelectRequest{
		Groups:                []string{"basic-users"},
		Username:              "alice",
		RequestedSubscription: "premium-sub", // Alice doesn't have access
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/subscriptions/select", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var response subscription.SelectResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if response.Error != "access_denied" {
		t.Errorf("expected error code 'access_denied', got %q", response.Error)
	}
}

func TestHandler_SelectSubscription_InvalidRequest(t *testing.T) {
	lister := &mockLister{subscriptions: nil}
	router := setupTestRouter(lister)

	req := httptest.NewRequest(http.MethodPost, "/subscriptions/select", bytes.NewBufferString("invalid json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var response subscription.SelectResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if response.Error != "bad_request" {
		t.Errorf("expected error code 'bad_request', got %q", response.Error)
	}
}

func TestHandler_SelectSubscription_UserWithoutGroups(t *testing.T) {
	// Create a subscription that matches by username instead of groups
	sub := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "maas.opendatahub.io/v1alpha1",
			"kind":       "MaaSSubscription",
			"metadata": map[string]any{
				"name":      "user-specific-sub",
				"namespace": "test-ns",
			},
			"spec": map[string]any{
				"owner": map[string]any{
					"users": []any{"specific-user"},
				},
				"priority": int64(10),
				"modelRefs": []any{
					map[string]any{
						"name": "test-model",
						"tokenRateLimits": []any{
							map[string]any{
								"limit": int64(1000),
							},
						},
					},
				},
				"tokenMetadata": map[string]any{
					"organizationId": "org-user",
					"costCenter":     "cc-user",
					"labels": map[string]any{
						"env": "test",
					},
				},
			},
		},
	}

	lister := &mockLister{subscriptions: []*unstructured.Unstructured{sub}}
	router := setupTestRouter(lister)

	// Test with empty groups array but valid username
	reqBody := subscription.SelectRequest{
		Groups:   []string{}, // Empty groups
		Username: "specific-user",
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/subscriptions/select", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d. Body: %s", w.Code, w.Body.String())
		return
	}

	var response subscription.SelectResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if response.Name != "user-specific-sub" {
		t.Errorf("expected subscription %q, got %q", "user-specific-sub", response.Name)
	}

	if response.OrganizationID != "org-user" {
		t.Errorf("expected orgID %q, got %q", "org-user", response.OrganizationID)
	}
}

func TestHandler_SelectSubscription_SingleSubscriptionAutoSelect(t *testing.T) {
	// Create a scenario where user only has access to one subscription
	subscriptions := []*unstructured.Unstructured{
		createTestSubscription("basic-sub", []string{"basic-users"}, 10, "org-basic", "cc-basic"),
		createTestSubscription("premium-sub", []string{"premium-users"}, 20, "org-premium", "cc-premium"),
		createTestSubscription("enterprise-sub", []string{"enterprise-users"}, 30, "org-enterprise", "cc-enterprise"),
	}

	lister := &mockLister{subscriptions: subscriptions}
	router := setupTestRouter(lister)

	tests := []struct {
		name          string
		groups        []string
		username      string
		expectedName  string
		expectedOrgID string
		expectedCode  int
		description   string
	}{
		{
			name:          "auto-select single accessible subscription - basic",
			groups:        []string{"basic-users"},
			username:      "alice",
			expectedName:  "basic-sub",
			expectedOrgID: "org-basic",
			expectedCode:  http.StatusOK,
			description:   "User only has access to basic-sub, should auto-select it",
		},
		{
			name:          "auto-select single accessible subscription - premium",
			groups:        []string{"premium-users"},
			username:      "bob",
			expectedName:  "premium-sub",
			expectedOrgID: "org-premium",
			expectedCode:  http.StatusOK,
			description:   "User only has access to premium-sub, should auto-select it",
		},
		{
			name:          "auto-select single accessible subscription - enterprise",
			groups:        []string{"enterprise-users"},
			username:      "charlie",
			expectedName:  "enterprise-sub",
			expectedOrgID: "org-enterprise",
			expectedCode:  http.StatusOK,
			description:   "User only has access to enterprise-sub, should auto-select it",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqBody := subscription.SelectRequest{
				Groups:   tt.groups,
				Username: tt.username,
			}
			jsonBody, err := json.Marshal(reqBody)
			if err != nil {
				t.Fatalf("failed to marshal request: %v", err)
			}

			req := httptest.NewRequest(http.MethodPost, "/subscriptions/select", bytes.NewBuffer(jsonBody))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			router.ServeHTTP(w, req)

			if w.Code != tt.expectedCode {
				t.Errorf("%s: expected status %d, got %d", tt.description, tt.expectedCode, w.Code)
			}

			if w.Code == http.StatusOK {
				var response subscription.SelectResponse
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
					t.Fatalf("failed to unmarshal response: %v", err)
				}

				if response.Name != tt.expectedName {
					t.Errorf("%s: expected subscription %q, got %q", tt.description, tt.expectedName, response.Name)
				}

				if response.OrganizationID != tt.expectedOrgID {
					t.Errorf("%s: expected orgID %q, got %q", tt.description, tt.expectedOrgID, response.OrganizationID)
				}
			}
		})
	}
}

func createTestSubscriptionWithLimit(name string, groups []string, priority int32, tokenLimit int64, orgID, costCenter string) *unstructured.Unstructured {
	groupsSlice := make([]any, len(groups))
	for i, g := range groups {
		groupsSlice[i] = map[string]any{"name": g}
	}

	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "maas.opendatahub.io/v1alpha1",
			"kind":       "MaaSSubscription",
			"metadata": map[string]any{
				"name":      name,
				"namespace": "test-ns",
			},
			"spec": map[string]any{
				"owner": map[string]any{
					"groups": groupsSlice,
				},
				"priority": int64(priority),
				"modelRefs": []any{
					map[string]any{
						"name": "test-model",
						"tokenRateLimits": []any{
							map[string]any{
								"limit":  tokenLimit,
								"window": "1m",
							},
						},
					},
				},
				"tokenMetadata": map[string]any{
					"organizationId": orgID,
					"costCenter":     costCenter,
					"labels": map[string]any{
						"env": "test",
					},
				},
			},
		},
	}
}

func TestHandler_SelectSubscription_MultipleSubscriptions(t *testing.T) {
	// Create subscriptions with different rate limits that both have the same group
	subscriptions := []*unstructured.Unstructured{
		createTestSubscriptionWithLimit("free-tier", []string{"system:authenticated"}, 10, 100, "org-free", "cc-free"),
		createTestSubscriptionWithLimit("premium-tier", []string{"system:authenticated"}, 10, 1000, "org-premium", "cc-premium"),
	}

	lister := &mockLister{subscriptions: subscriptions}
	router := setupTestRouter(lister)

	tests := []struct {
		name                  string
		groups                []string
		username              string
		requestedSubscription string
		expectedName          string
		expectedOrgID         string
		expectedError         string
		description           string
	}{
		{
			name:          "multiple subscriptions without explicit selection",
			groups:        []string{"system:authenticated"},
			username:      "alice",
			expectedError: "multiple_subscriptions",
			description:   "User has access to both free and premium. Should return error requiring explicit selection.",
		},
		{
			name:                  "explicit selection with multiple available",
			groups:                []string{"system:authenticated"},
			username:              "bob",
			requestedSubscription: "free-tier",
			expectedName:          "free-tier",
			expectedOrgID:         "org-free",
			description:           "User explicitly requests free tier despite premium being available. Should honor explicit selection.",
		},
		{
			name:                  "explicit selection of premium with multiple available",
			groups:                []string{"system:authenticated"},
			username:              "charlie",
			requestedSubscription: "premium-tier",
			expectedName:          "premium-tier",
			expectedOrgID:         "org-premium",
			description:           "User explicitly requests premium tier. Should honor explicit selection.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqBody := subscription.SelectRequest{
				Groups:                tt.groups,
				Username:              tt.username,
				RequestedSubscription: tt.requestedSubscription,
			}
			jsonBody, err := json.Marshal(reqBody)
			if err != nil {
				t.Fatalf("failed to marshal request: %v", err)
			}

			req := httptest.NewRequest(http.MethodPost, "/subscriptions/select", bytes.NewBuffer(jsonBody))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			router.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("%s: expected status 200, got %d", tt.description, w.Code)
			}

			var response subscription.SelectResponse
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatalf("failed to unmarshal response: %v", err)
			}

			if tt.expectedError != "" {
				// Expecting an error response
				if response.Error != tt.expectedError {
					t.Errorf("%s: expected error code %q, got %q", tt.description, tt.expectedError, response.Error)
				}
			} else {
				// Expecting a success response
				if response.Name != tt.expectedName {
					t.Errorf("%s: expected subscription %q, got %q", tt.description, tt.expectedName, response.Name)
				}

				if response.OrganizationID != tt.expectedOrgID {
					t.Errorf("%s: expected orgID %q, got %q", tt.description, tt.expectedOrgID, response.OrganizationID)
				}
			}
		})
	}
}
