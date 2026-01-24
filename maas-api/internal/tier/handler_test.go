package tier_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/tier"
	"github.com/opendatahub-io/models-as-a-service/maas-api/test/fixtures"
)

// createTestMapper wraps the unified fixtures function for backward compatibility.
func createTestMapper(withConfigMap bool) *tier.Mapper {
	return fixtures.CreateTestMapper(withConfigMap)
}

func TestHandler_PostTierLookup_Success(t *testing.T) {
	mapper := createTestMapper(true)
	router := fixtures.SetupTierTestRouter(mapper)

	tests := []struct {
		name                string
		groups              []string
		expectedTier        string
		expectedDisplayName string
		expectedCode        int
	}{
		{
			name:                "single group - free tier",
			groups:              []string{"system:authenticated"},
			expectedTier:        "free",
			expectedDisplayName: "Free Tier",
			expectedCode:        http.StatusOK,
		},
		{
			name:                "single group - premium tier",
			groups:              []string{"tier-premium-users"},
			expectedTier:        "premium",
			expectedDisplayName: "Premium Tier",
			expectedCode:        http.StatusOK,
		},
		{
			name:                "single group - enterprise tier",
			groups:              []string{"tier-enterprise-users"},
			expectedTier:        "enterprise",
			expectedDisplayName: "Enterprise Tier",
			expectedCode:        http.StatusOK,
		},
		{
			name:                "multiple groups - enterprise wins over free",
			groups:              []string{"system:authenticated", "tier-enterprise-users"},
			expectedTier:        "enterprise",
			expectedDisplayName: "Enterprise Tier",
			expectedCode:        http.StatusOK,
		},
		{
			name:                "multiple groups - premium wins over free",
			groups:              []string{"system:authenticated", "tier-premium-users"},
			expectedTier:        "premium",
			expectedDisplayName: "Premium Tier",
			expectedCode:        http.StatusOK,
		},
		{
			name:                "all tiers - enterprise wins",
			groups:              []string{"system:authenticated", "tier-premium-users", "admin-users"},
			expectedTier:        "enterprise",
			expectedDisplayName: "Enterprise Tier",
			expectedCode:        http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqBody := tier.LookupRequest{Groups: tt.groups}
			jsonBody, err := json.Marshal(reqBody)
			if err != nil {
				t.Fatalf("failed to marshal request body: %v", err)
			}

			w := httptest.NewRecorder()
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "/tiers/lookup", bytes.NewBuffer(jsonBody))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(w, req)

			if w.Code != tt.expectedCode {
				t.Errorf("expected status %d, got %d", tt.expectedCode, w.Code)
			}

			var response tier.LookupResponse
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatalf("failed to unmarshal response: %v", err)
			}

			if response.Tier != tt.expectedTier {
				t.Errorf("expected tier %s, got %s", tt.expectedTier, response.Tier)
			}

			if response.DisplayName != tt.expectedDisplayName {
				t.Errorf("expected displayName %s, got %s", tt.expectedDisplayName, response.DisplayName)
			}
		})
	}
}

func TestHandler_PostTierLookup_GroupNotFound(t *testing.T) {
	mapper := createTestMapper(true)
	router := fixtures.SetupTierTestRouter(mapper)

	reqBody := tier.LookupRequest{Groups: []string{"unknown-group"}}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "/tiers/lookup", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, w.Code)
	}

	var response tier.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to unmarshal error response: %v", err)
	}

	if response.Error != "not_found" {
		t.Errorf("expected error 'not_found', got '%s'", response.Error)
	}
}

func TestHandler_PostTierLookup_BadRequest(t *testing.T) {
	mapper := createTestMapper(true)
	router := fixtures.SetupTierTestRouter(mapper)

	tests := []struct {
		name        string
		requestBody string
		description string
	}{
		{
			name:        "empty request body",
			requestBody: "",
			description: "No JSON body provided",
		},
		{
			name:        "invalid JSON",
			requestBody: "{invalid json}",
			description: "Malformed JSON body",
		},
		{
			name:        "missing groups field",
			requestBody: "{}",
			description: "Request without groups field",
		},
		{
			name:        "empty groups array",
			requestBody: `{"groups": []}`,
			description: "Empty groups array",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "/tiers/lookup", bytes.NewBufferString(tt.requestBody))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
			}

			var response tier.ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatalf("failed to unmarshal error response: %v", err)
			}

			if response.Error != "bad_request" {
				t.Errorf("expected error 'bad_request', got '%s'", response.Error)
			}
		})
	}
}

func TestHandler_PostTierLookup_ConfigMapMissing_ShouldError(t *testing.T) {
	mapper := createTestMapper(false) // No ConfigMap
	router := fixtures.SetupTierTestRouter(mapper)

	reqBody := tier.LookupRequest{Groups: []string{"any-group"}}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "/tiers/lookup", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Errorf("expected error status, got %d", w.Code)
	}
}

func TestHandler_PostTierLookup_DisplayNameFallback(t *testing.T) {
	testLogger := logger.Development()
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      constant.TierMappingConfigMap,
			Namespace: fixtures.TestNamespace,
		},
		Data: map[string]string{
			"tiers": `
- name: basic
  level: 1
  groups:
  - basic-users
`,
		},
	}

	mapper := tier.NewMapper(testLogger, fixtures.NewConfigMapLister(configMap), fixtures.TestTenant, fixtures.TestNamespace)
	router := fixtures.SetupTierTestRouter(mapper)

	reqBody := tier.LookupRequest{Groups: []string{"basic-users"}}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "/tiers/lookup", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var response tier.LookupResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	// displayName should fall back to name when not provided
	if response.DisplayName != "basic" {
		t.Errorf("expected displayName to fall back to 'basic', got %s", response.DisplayName)
	}
}
