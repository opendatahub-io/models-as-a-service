package token_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/constant"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
	"github.com/opendatahub-io/models-as-a-service/maas-api/test/fixtures"
)

func TestAPIEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)

	zapLog, _ := zap.NewDevelopment()
	testLogger := zapr.NewLogger(zapLog)
	manager, _, cleanup := fixtures.StubTokenProviderAPIs(t, true)
	defer cleanup()

	handler := token.NewHandler(testLogger, "test", manager)

	router := gin.New()
	router.Use(handler.ExtractUserInfo())
	router.POST("/v1/tokens", handler.IssueToken)

	tests := []struct {
		name            string
		username        string
		group           string
		expectedStatus  int
		expectedError   string
		expectedCode    string
		expectedRefId   string
		shouldHaveToken bool
		description     string
	}{
		{
			name:            "Ephemeral",
			username:        "test-user",
			group:           `["maas-users","system:authenticated"]`,
			expectedStatus:  http.StatusCreated,
			shouldHaveToken: true,
			description:     "Valid headers should issue token",
		},
		{
			name:           "Missing Username Header",
			username:       "",
			group:          `["maas-users"]`,
			expectedStatus: http.StatusInternalServerError,
			expectedError:  "Exception thrown while generating token",
			expectedCode:   "AUTH_FAILURE",
			expectedRefId:  "001",
			description:    "Missing username header should return 500",
		},
		{
			name:           "Missing Group Header",
			username:       "test-user",
			group:          "",
			expectedStatus: http.StatusInternalServerError,
			expectedError:  "Exception thrown while generating token",
			expectedCode:   "AUTH_FAILURE",
			expectedRefId:  "002",
			description:    "Missing group header should return 500",
		},
		{
			name:           "Invalid Group Header Format",
			username:       "test-user",
			group:          "invalid-format",
			expectedStatus: http.StatusInternalServerError,
			expectedError:  "Exception thrown while generating token",
			expectedCode:   "AUTH_FAILURE",
			expectedRefId:  "003",
			description:    "Invalid group header format should return 500",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqBody, _ := json.Marshal(map[string]any{})
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/tokens", bytes.NewBuffer(reqBody))

			// Set headers based on test case (using canonical format)
			if tt.username != "" {
				req.Header.Set(constant.HeaderUsername, tt.username)
			}
			if tt.group != "" {
				req.Header.Set(constant.HeaderGroup, tt.group)
			}

			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d. Description: %s. Body: %s", tt.expectedStatus, w.Code, tt.description, w.Body.String())
			}

			var response map[string]any
			err := json.Unmarshal(w.Body.Bytes(), &response)
			require.NoError(t, err)

			if tt.shouldHaveToken {
				if response["token"] == nil || response["token"] == "" {
					t.Errorf("expected non-empty token. Description: %s", tt.description)
				}
			} else {
				if response["error"] == nil {
					t.Errorf("expected error. Description: %s", tt.description)
				}
				errorMsg, ok := response["error"].(string)
				if !ok {
					t.Errorf("expected error to be a string, got %T", response["error"])
				} else if errorMsg != tt.expectedError {
					t.Errorf("expected error message: '%s'; got: '%s'. Description: %s", tt.expectedError, errorMsg, tt.description)
				}
				if tt.expectedCode != "" {
					exceptionCode, ok := response["exceptionCode"].(string)
					if !ok {
						t.Errorf("expected exceptionCode to be a string, got %T", response["exceptionCode"])
					} else if exceptionCode != tt.expectedCode {
						t.Errorf("expected exceptionCode: '%s'; got: '%s'. Description: %s", tt.expectedCode, exceptionCode, tt.description)
					}
				}
				if tt.expectedRefId != "" {
					refId, ok := response["refId"].(string)
					if !ok {
						t.Errorf("expected refId to be a string, got %T", response["refId"])
					} else if refId != tt.expectedRefId {
						t.Errorf("expected refId: '%s'; got: '%s'. Description: %s", tt.expectedRefId, refId, tt.description)
					}
				}
			}
		})
	}
}
