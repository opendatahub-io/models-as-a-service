package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/handlers"
)

type databaseHealthReporter struct{}

func (databaseHealthReporter) DatabaseHealth() map[string]any {
	return map[string]any{"read_replica_configured": true, "read_pool_healthy": false}
}

func TestHealthCheckIncludesNonSecretDatabaseStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/health", handlers.NewHealthHandler().
		WithDatabaseHealth(databaseHealthReporter{}).HealthCheck)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.JSONEq(t, `{
		"status": "healthy",
		"database": {
			"read_replica_configured": true,
			"read_pool_healthy": false
		}
	}`, recorder.Body.String())
}
