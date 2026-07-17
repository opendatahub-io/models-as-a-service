package middleware_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/middleware"
)

func TestBodyLimit_AcceptsSmallBody(t *testing.T) {
	router := gin.New()
	router.Use(middleware.BodyLimit())
	router.POST("/test", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.Status(http.StatusRequestEntityTooLarge)
			return
		}
		c.String(http.StatusOK, string(body))
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(`{"name":"test"}`))
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"name":"test"}`, w.Body.String())
}

func TestBodyLimit_RejectsOversizedBody(t *testing.T) {
	router := gin.New()
	router.Use(middleware.BodyLimit())
	router.POST("/test", func(c *gin.Context) {
		var req map[string]any
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
			return
		}
		c.Status(http.StatusOK)
	})

	// Valid JSON whose content exceeds the 1 MiB limit, forcing the JSON
	// decoder to read past MaxBytesReader's threshold and trigger
	// *http.MaxBytesError (not just a JSON syntax error).
	oversized := `{"data":"` + strings.Repeat("A", 2<<20) + `"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(oversized))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

func TestBodyLimit_AllowsGetWithoutBody(t *testing.T) {
	router := gin.New()
	router.Use(middleware.BodyLimit())
	router.GET("/test", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestBodyLimit_ExactlyAtLimit(t *testing.T) {
	router := gin.New()
	router.Use(middleware.BodyLimit())
	router.POST("/test", func(c *gin.Context) {
		_, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.Status(http.StatusRequestEntityTooLarge)
			return
		}
		c.Status(http.StatusOK)
	})

	// Exactly 1 MiB should be accepted
	body := strings.Repeat("A", 1<<20)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body))
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
}
