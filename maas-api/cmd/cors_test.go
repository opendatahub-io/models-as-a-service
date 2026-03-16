package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestIsLocalhostOrigin(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		want   bool
	}{
		{name: "http localhost with port", origin: "http://localhost:3000", want: true},
		{name: "https localhost with port", origin: "https://localhost:8443", want: true},
		{name: "http 127.0.0.1 with port", origin: "http://127.0.0.1:8080", want: true},
		{name: "https 127.0.0.1 with port", origin: "https://127.0.0.1:443", want: true},
		{name: "http localhost default port", origin: "http://localhost", want: true},
		{name: "https localhost default port", origin: "https://localhost", want: true},
		{name: "http 127.0.0.1 default port", origin: "http://127.0.0.1", want: true},
		{name: "https 127.0.0.1 default port", origin: "https://127.0.0.1", want: true},
		{name: "loopback range 127.0.0.2", origin: "http://127.0.0.2:8080", want: true},
		{name: "loopback range 127.255.255.254", origin: "http://127.255.255.254:9090", want: true},

		{name: "external origin", origin: "https://external.com", want: false},
		{name: "external with localhost in path", origin: "https://external.com/localhost:3000", want: false},
		{name: "localhost without scheme", origin: "localhost:3000", want: false},
		{name: "subdomain of localhost", origin: "http://foo.localhost:3000", want: false},
		{name: "non-loopback IP", origin: "http://192.168.1.1:8080", want: false},
		{name: "ftp scheme localhost", origin: "ftp://localhost", want: false},
		{name: "empty string", origin: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isLocalhostOrigin(tt.origin))
		})
	}
}

func newCORSTestRouter(useCORS bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	if useCORS {
		router.Use(cors.New(debugCORSConfig()))
	}
	router.OPTIONS("/*path", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	router.GET("/test", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	return router
}

func TestDebugCORS_AllowsLocalhostOrigin(t *testing.T) {
	router := newCORSTestRouter(true)

	origins := []string{
		"http://localhost:3000",
		"https://localhost:8443",
		"http://127.0.0.1:8080",
		"https://127.0.0.1:443",
		"http://localhost",
		"https://localhost",
		"http://127.0.0.1",
		"https://127.0.0.1",
	}

	for _, origin := range origins {
		t.Run(origin, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			req.Header.Set("Origin", origin)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, origin, w.Header().Get("Access-Control-Allow-Origin"),
				"expected CORS to allow localhost origin")
		})
	}
}

func TestDebugCORS_RejectsExternalOrigin(t *testing.T) {
	router := newCORSTestRouter(true)

	origins := []string{
		"https://external.com",
		"https://attacker.example.org",
		"http://not-localhost:3000",
		"http://192.168.1.1:8080",
	}

	for _, origin := range origins {
		t.Run(origin, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			req.Header.Set("Origin", origin)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			assert.Equal(t, http.StatusForbidden, w.Code,
				"cross-origin request from non-localhost should be rejected")
			assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"),
				"expected CORS to reject non-localhost origin")
		})
	}
}

func TestDebugCORS_PreflightAllowsLocalhostOrigin(t *testing.T) {
	router := newCORSTestRouter(true)

	req := httptest.NewRequest(http.MethodOptions, "/test", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Authorization")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, "http://localhost:3000", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, w.Header().Get("Access-Control-Allow-Methods"), "POST")
	assert.Contains(t, w.Header().Get("Access-Control-Allow-Headers"), "Authorization")
}

func TestDebugCORS_PreflightRejectsExternalOrigin(t *testing.T) {
	router := newCORSTestRouter(true)

	req := httptest.NewRequest(http.MethodOptions, "/test", nil)
	req.Header.Set("Origin", "https://external.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"),
		"preflight should not return CORS headers for external origin")
}

func TestDebugCORS_CredentialsNotAllowed(t *testing.T) {
	router := newCORSTestRouter(true)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Empty(t, w.Header().Get("Access-Control-Allow-Credentials"),
		"credentials should not be allowed — API uses Bearer tokens, not cookies")
}

func TestDebugCORS_SameOriginRequestPassesThrough(t *testing.T) {
	router := newCORSTestRouter(true)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code,
		"same-origin request (no Origin header) must not be blocked by CORS middleware")
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"),
		"no CORS headers expected for same-origin request")
}

func TestNoCORS_WhenDebugModeDisabled(t *testing.T) {
	router := newCORSTestRouter(false)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"),
		"CORS headers should not be present when debug mode is off")
}
