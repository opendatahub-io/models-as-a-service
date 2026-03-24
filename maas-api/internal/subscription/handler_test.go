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
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
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
								"limit":  int64(1000),
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

	testAccessDenied := func(t *testing.T, requestedSubscription, expectedError string) {
		t.Helper()
		reqBody := subscription.SelectRequest{
			Groups:                []string{"basic-users"},
			Username:              "alice",
			RequestedSubscription: requestedSubscription,
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

		if response.Error != expectedError {
			t.Errorf("expected error code %q, got %q", expectedError, response.Error)
		}
	}

	t.Run("bare name without access returns access_denied", func(t *testing.T) {
		testAccessDenied(t, "premium-sub", "access_denied")
	})

	t.Run("qualified name without access returns access_denied", func(t *testing.T) {
		testAccessDenied(t, "test-ns/premium-sub", "access_denied")
	})
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
								"limit":  int64(1000),
								"window": "1m",
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

// createTestSubscriptionWithModels creates a subscription with specific model references.
// All test subscriptions use "tenant-a" namespace.
func createTestSubscriptionWithModels(
	name string, groups []string,
	models []struct{ ns, name string },
	priority int32, orgID, costCenter string,
) *unstructured.Unstructured {
	groupsSlice := make([]any, len(groups))
	for i, g := range groups {
		groupsSlice[i] = map[string]any{"name": g}
	}

	modelRefsSlice := make([]any, len(models))
	for i, m := range models {
		modelRefsSlice[i] = map[string]any{
			"namespace": m.ns,
			"name":      m.name,
			"tokenRateLimits": []any{
				map[string]any{
					"limit":  int64(1000),
					"window": "1m",
				},
			},
		}
	}

	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "maas.opendatahub.io/v1alpha1",
			"kind":       "MaaSSubscription",
			"metadata": map[string]any{
				"name":      name,
				"namespace": "tenant-a",
			},
			"spec": map[string]any{
				"owner": map[string]any{
					"groups": groupsSlice,
				},
				"priority":  int64(priority),
				"modelRefs": modelRefsSlice,
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

// --- Subscription && /v1/model/:model-id/subscriptions endpoints tests ---

func createTestSubscriptionWithAnnotations(name string, groups []string, modelNames []string, annotations map[string]string) *unstructured.Unstructured {
	groupsSlice := make([]any, len(groups))
	for i, g := range groups {
		groupsSlice[i] = map[string]any{"name": g}
	}

	modelRefs := make([]any, len(modelNames))
	for i, m := range modelNames {
		modelRefs[i] = map[string]any{
			"name": m,
			"tokenRateLimits": []any{
				map[string]any{"limit": int64(1000), "window": "1m"},
			},
		}
	}

	metadata := map[string]any{
		"name":      name,
		"namespace": "test-ns",
	}
	if len(annotations) > 0 {
		annMap := make(map[string]any, len(annotations))
		for k, v := range annotations {
			annMap[k] = v
		}
		metadata["annotations"] = annMap
	}

	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "maas.opendatahub.io/v1alpha1",
			"kind":       "MaaSSubscription",
			"metadata":   metadata,
			"spec": map[string]any{
				"owner": map[string]any{
					"groups": groupsSlice,
				},
				"priority":  int64(10),
				"modelRefs": modelRefs,
			},
		},
	}
}

// runSelectSubscriptionTest executes a subscription selection test case.
func runSelectSubscriptionTest(
	t *testing.T, router *gin.Engine,
	groups []string, username, requestedSubscription, requestedModel string,
	expectedName, expectedError, description string,
) {
	t.Helper()

	reqBody := subscription.SelectRequest{
		Groups:                groups,
		Username:              username,
		RequestedSubscription: requestedSubscription,
		RequestedModel:        requestedModel,
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
		t.Errorf("%s: expected status 200, got %d", description, w.Code)
	}

	var response subscription.SelectResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if expectedError != "" {
		// Expecting an error response
		if response.Error != expectedError {
			t.Errorf("%s: expected error code %q, got %q. Message: %s", description, expectedError, response.Error, response.Message)
		}
	} else {
		// Expecting a success response
		if response.Name != expectedName {
			t.Errorf("%s: expected subscription %q, got %q", description, expectedName, response.Name)
		}
	}
}

// TestHandler_SelectSubscription_ModelBasedAutoSelection tests auto-selection based on model availability.
func TestHandler_SelectSubscription_ModelBasedAutoSelection(t *testing.T) {
	// Create subscriptions with different models
	subscriptions := []*unstructured.Unstructured{
		createTestSubscriptionWithModels("gold", []string{"premium-users"}, []struct{ ns, name string }{
			{ns: "models", name: "llm"},
			{ns: "models", name: "embedding"},
		}, 10, "org-gold", "cc-gold"),
		createTestSubscriptionWithModels("silver", []string{"premium-users"}, []struct{ ns, name string }{
			{ns: "models", name: "small-model"},
		}, 10, "org-silver", "cc-silver"),
	}

	lister := &mockLister{subscriptions: subscriptions}
	router := setupTestRouter(lister)

	tests := []struct {
		name           string
		groups         []string
		username       string
		requestedModel string
		expectedName   string
		expectedError  string
		description    string
	}{
		{
			name:           "auto-select when only gold has llm model",
			groups:         []string{"premium-users"},
			username:       "alice",
			requestedModel: "models/llm",
			expectedName:   "gold",
			description:    "User has access to both gold and silver, but only gold has models/llm. Should auto-select gold.",
		},
		{
			name:           "auto-select when only silver has small-model",
			groups:         []string{"premium-users"},
			username:       "alice",
			requestedModel: "models/small-model",
			expectedName:   "silver",
			description:    "User has access to both gold and silver, but only silver has models/small-model. Should auto-select silver.",
		},
		{
			name:           "auto-select when only gold has embedding model",
			groups:         []string{"premium-users"},
			username:       "alice",
			requestedModel: "models/embedding",
			expectedName:   "gold",
			description:    "User has access to both gold and silver, but only gold has models/embedding. Should auto-select gold.",
		},
		{
			name:           "error when no subscription has the requested model",
			groups:         []string{"premium-users"},
			username:       "alice",
			requestedModel: "models/nonexistent",
			expectedError:  "not_found",
			description:    "User has access to gold and silver, but neither has models/nonexistent. Should return not_found error.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqBody := subscription.SelectRequest{
				Groups:         tt.groups,
				Username:       tt.username,
				RequestedModel: tt.requestedModel,
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
			}
		})
	}
}

// TestHandler_SelectSubscription_ModelValidation tests that explicit subscription selection validates model access.
func TestHandler_SelectSubscription_ModelValidation(t *testing.T) {
	// Create subscriptions with different models
	subscriptions := []*unstructured.Unstructured{
		createTestSubscriptionWithModels("gold", []string{"premium-users"}, []struct{ ns, name string }{
			{ns: "models", name: "llm"},
		}, 10, "org-gold", "cc-gold"),
		createTestSubscriptionWithModels("silver", []string{"premium-users"}, []struct{ ns, name string }{
			{ns: "models", name: "small-model"},
		}, 10, "org-silver", "cc-silver"),
	}

	lister := &mockLister{subscriptions: subscriptions}
	router := setupTestRouter(lister)

	tests := []struct {
		name                  string
		groups                []string
		username              string
		requestedSubscription string
		requestedModel        string
		expectedName          string
		expectedError         string
		description           string
	}{
		{
			name:                  "explicit selection with correct model",
			groups:                []string{"premium-users"},
			username:              "alice",
			requestedSubscription: "gold",
			requestedModel:        "models/llm",
			expectedName:          "gold",
			description:           "User explicitly selects gold subscription which has models/llm. Should succeed.",
		},
		{
			name:                  "explicit selection with wrong model",
			groups:                []string{"premium-users"},
			username:              "alice",
			requestedSubscription: "silver",
			requestedModel:        "models/llm",
			expectedError:         "model_not_in_subscription",
			description:           "User explicitly selects silver subscription but it doesn't have models/llm. Should return model_not_in_subscription error.",
		},
		{
			name:                  "explicit selection gold with small-model",
			groups:                []string{"premium-users"},
			username:              "alice",
			requestedSubscription: "gold",
			requestedModel:        "models/small-model",
			expectedError:         "model_not_in_subscription",
			description:           "User explicitly selects gold subscription but it doesn't have models/small-model. Should return model_not_in_subscription error.",
		},
		{
			name:                  "explicit selection silver with small-model",
			groups:                []string{"premium-users"},
			username:              "alice",
			requestedSubscription: "silver",
			requestedModel:        "models/small-model",
			expectedName:          "silver",
			description:           "User explicitly selects silver subscription which has models/small-model. Should succeed.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runSelectSubscriptionTest(
				t, router,
				tt.groups, tt.username, tt.requestedSubscription, tt.requestedModel,
				tt.expectedName, tt.expectedError, tt.description,
			)
		})
	}
}

// TestHandler_SelectSubscription_MultipleSubscriptionsSameModel tests behavior when multiple subscriptions have the same model.
func TestHandler_SelectSubscription_MultipleSubscriptionsSameModel(t *testing.T) {
	// Create two subscriptions that both have the same model
	subscriptions := []*unstructured.Unstructured{
		createTestSubscriptionWithModels("gold", []string{"premium-users"}, []struct{ ns, name string }{
			{ns: "models", name: "llm"},
		}, 10, "org-gold", "cc-gold"),
		createTestSubscriptionWithModels("platinum", []string{"premium-users"}, []struct{ ns, name string }{
			{ns: "models", name: "llm"},
		}, 20, "org-platinum", "cc-platinum"),
	}

	lister := &mockLister{subscriptions: subscriptions}
	router := setupTestRouter(lister)

	tests := []struct {
		name                  string
		groups                []string
		username              string
		requestedSubscription string
		requestedModel        string
		expectedName          string
		expectedError         string
		description           string
	}{
		{
			name:           "error when both subscriptions have the model",
			groups:         []string{"premium-users"},
			username:       "alice",
			requestedModel: "models/llm",
			expectedError:  "multiple_subscriptions",
			description:    "User has access to both gold and platinum, and both have models/llm. Should require explicit selection.",
		},
		{
			name:                  "explicit selection works when both have model",
			groups:                []string{"premium-users"},
			username:              "alice",
			requestedSubscription: "gold",
			requestedModel:        "models/llm",
			expectedName:          "gold",
			description:           "User explicitly selects gold when both subscriptions have the model. Should honor explicit selection.",
		},
		{
			name:                  "explicit selection of higher priority subscription",
			groups:                []string{"premium-users"},
			username:              "alice",
			requestedSubscription: "platinum",
			requestedModel:        "models/llm",
			expectedName:          "platinum",
			description:           "User explicitly selects platinum (higher priority) when both subscriptions have the model. Should honor explicit selection.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runSelectSubscriptionTest(
				t, router,
				tt.groups, tt.username, tt.requestedSubscription, tt.requestedModel,
				tt.expectedName, tt.expectedError, tt.description,
			)
		})
	}
}

func setupListTestRouter(lister subscription.Lister, username string, groups []string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	log := logger.New(false)
	selector := subscription.NewSelector(log, lister)
	handler := subscription.NewHandler(log, selector)

	setUser := func(c *gin.Context) {
		c.Set("user", &token.UserContext{
			Username: username,
			Groups:   groups,
		})
		c.Next()
	}

	router.GET("/v1/subscriptions", setUser, handler.ListSubscriptions)
	router.GET("/v1/model/:model-id/subscriptions", setUser, handler.ListSubscriptionsForModel)
	return router
}

func TestListSubscriptions_MultipleAccessible(t *testing.T) {
	lister := &mockLister{subscriptions: []*unstructured.Unstructured{
		createTestSubscriptionWithAnnotations("free-sub", []string{"free-users"}, []string{"model-a"}, map[string]string{
			"openshift.io/display-name": "Free Tier",
		}),
		createTestSubscriptionWithAnnotations("premium-sub", []string{"premium-users"}, []string{"model-a", "model-b"}, map[string]string{
			"openshift.io/display-name": "Premium Plan",
			"openshift.io/description":  "High limits for production",
		}),
		createTestSubscriptionWithAnnotations("other-sub", []string{"other-group"}, []string{"model-a"}, nil),
	}}

	router := setupListTestRouter(lister, "alice", []string{"free-users", "premium-users"})
	req := httptest.NewRequest(http.MethodGet, "/v1/subscriptions", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var result []subscription.SubscriptionInfo
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("expected 2 subscriptions, got %d", len(result))
	}

	found := map[string]string{}
	for _, s := range result {
		found[s.SubscriptionIDHeader] = s.SubscriptionDescription
	}

	if desc, ok := found["free-sub"]; !ok || desc != "Free Tier" {
		t.Errorf("expected free-sub with description 'Free Tier' (fallback from display-name), got %q", desc)
	}
	if desc, ok := found["premium-sub"]; !ok || desc != "High limits for production" {
		t.Errorf("expected premium-sub with description 'High limits for production', got %q", desc)
	}
}

func TestListSubscriptions_NoAccess(t *testing.T) {
	lister := &mockLister{subscriptions: []*unstructured.Unstructured{
		createTestSubscriptionWithAnnotations("premium-sub", []string{"premium-users"}, []string{"model-a"}, nil),
	}}

	router := setupListTestRouter(lister, "nobody", []string{"unknown-group"})
	req := httptest.NewRequest(http.MethodGet, "/v1/subscriptions", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var result []subscription.SubscriptionInfo
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if len(result) != 0 {
		t.Errorf("expected empty array, got %d items", len(result))
	}
}

func TestListSubscriptionsForModel_FiltersByModel(t *testing.T) {
	lister := &mockLister{subscriptions: []*unstructured.Unstructured{
		createTestSubscriptionWithAnnotations("free-sub", []string{"free-users"}, []string{"model-a"}, map[string]string{
			"openshift.io/display-name": "Free Tier",
		}),
		createTestSubscriptionWithAnnotations("premium-sub", []string{"premium-users"}, []string{"model-a", "model-b"}, map[string]string{
			"openshift.io/display-name": "Premium Plan",
		}),
	}}

	router := setupListTestRouter(lister, "alice", []string{"free-users", "premium-users"})

	// model-b is only in premium-sub
	req := httptest.NewRequest(http.MethodGet, "/v1/model/model-b/subscriptions", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var result []subscription.SubscriptionInfo
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("expected 1 subscription for model-b, got %d", len(result))
	}
	if result[0].SubscriptionIDHeader != "premium-sub" {
		t.Errorf("expected premium-sub, got %q", result[0].SubscriptionIDHeader)
	}
}

func TestListSubscriptionsForModel_UnknownModel(t *testing.T) {
	lister := &mockLister{subscriptions: []*unstructured.Unstructured{
		createTestSubscriptionWithAnnotations("free-sub", []string{"free-users"}, []string{"model-a"}, nil),
	}}

	router := setupListTestRouter(lister, "alice", []string{"free-users"})

	req := httptest.NewRequest(http.MethodGet, "/v1/model/nonexistent-model/subscriptions", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var result []subscription.SubscriptionInfo
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if len(result) != 0 {
		t.Errorf("expected empty array for unknown model, got %d items", len(result))
	}
}

func TestListSubscriptions_DescriptionFallback(t *testing.T) {
	lister := &mockLister{subscriptions: []*unstructured.Unstructured{
		createTestSubscriptionWithAnnotations("both-annotations", []string{"free-users"}, []string{"m"}, map[string]string{
			"openshift.io/display-name": "My Display Name",
			"openshift.io/description":  "My Description",
		}),
		createTestSubscriptionWithAnnotations("with-description-only", []string{"free-users"}, []string{"m"}, map[string]string{
			"openshift.io/description": "Description Only",
		}),
		createTestSubscriptionWithAnnotations("with-display-name-only", []string{"free-users"}, []string{"m"}, map[string]string{
			"openshift.io/display-name": "Display Name Only",
		}),
		createTestSubscriptionWithAnnotations("no-annotations", []string{"free-users"}, []string{"m"}, nil),
	}}

	router := setupListTestRouter(lister, "alice", []string{"free-users"})
	req := httptest.NewRequest(http.MethodGet, "/v1/subscriptions", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var result []subscription.SubscriptionInfo
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if len(result) != 4 {
		t.Fatalf("expected 4 subscriptions, got %d", len(result))
	}

	byID := map[string]subscription.SubscriptionInfo{}
	for _, s := range result {
		byID[s.SubscriptionIDHeader] = s
	}

	// Description preferred over display-name
	if byID["both-annotations"].SubscriptionDescription != "My Description" {
		t.Errorf("expected description 'My Description', got %q", byID["both-annotations"].SubscriptionDescription)
	}
	if byID["both-annotations"].DisplayName != "My Display Name" {
		t.Errorf("expected display_name 'My Display Name', got %q", byID["both-annotations"].DisplayName)
	}
	// Description only
	if byID["with-description-only"].SubscriptionDescription != "Description Only" {
		t.Errorf("expected description 'Description Only', got %q", byID["with-description-only"].SubscriptionDescription)
	}
	// Display-name falls back to subscription_description when no description
	if byID["with-display-name-only"].SubscriptionDescription != "Display Name Only" {
		t.Errorf("expected description fallback to display-name, got %q", byID["with-display-name-only"].SubscriptionDescription)
	}
	if byID["with-display-name-only"].DisplayName != "Display Name Only" {
		t.Errorf("expected display_name 'Display Name Only', got %q", byID["with-display-name-only"].DisplayName)
	}
	// No annotations: falls back to name
	if byID["no-annotations"].SubscriptionDescription != "no-annotations" {
		t.Errorf("expected name fallback, got %q", byID["no-annotations"].SubscriptionDescription)
	}
}

func TestListSubscriptionsForModel_NoAccess(t *testing.T) {
	lister := &mockLister{subscriptions: []*unstructured.Unstructured{
		createTestSubscriptionWithAnnotations("premium-sub", []string{"premium-users"}, []string{"model-a"}, nil),
	}}

	router := setupListTestRouter(lister, "nobody", []string{"unknown-group"})
	req := httptest.NewRequest(http.MethodGet, "/v1/model/model-a/subscriptions", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var result []subscription.SubscriptionInfo
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if len(result) != 0 {
		t.Errorf("expected empty array when user has no access, got %d items", len(result))
	}
}
