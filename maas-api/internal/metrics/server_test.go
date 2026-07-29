package metrics_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/metrics"
)

func TestMetricsServerIntegration(t *testing.T) {
	reg := prometheus.NewRegistry()
	recorder, err := metrics.NewPrometheusRecorder(reg)
	require.NoError(t, err)

	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)
	port := tcpAddr.Port
	require.NoError(t, listener.Close())

	srv, err := metrics.NewMetricsServer(metrics.ServerOptions{
		Addr:   fmt.Sprintf(":%d", port),
		Reg:    reg,
		Secure: false,
	})
	require.NoError(t, err)
	go func() {
		if err := metrics.ListenAndServe(srv); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("metrics server error: %v", err)
		}
	}()
	t.Cleanup(func() { _ = srv.Close() })

	client := &http.Client{Timeout: 2 * time.Second}

	require.Eventually(t, func() bool {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/metrics", port), nil)
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 2*time.Second, 50*time.Millisecond)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(metrics.NewMiddleware(recorder, "test-tenant"))
	router.GET("/v1/models", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/models", nil)
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	scrapeReq, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/metrics", port), nil)
	resp, err := client.Do(scrapeReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	bodyStr := string(body)

	assert.Contains(t, bodyStr, `maas_api_http_requests_total{method="GET",route="/v1/models",status="200",tenant_name="test-tenant"} 1`)
	assert.Contains(t, bodyStr, "maas_api_http_request_duration_seconds")

	// Verify new metrics are also present
	assert.Contains(t, bodyStr, `maas_requests_total 1`)
	assert.Contains(t, bodyStr, "maas_request_duration_seconds")
	assert.Contains(t, bodyStr, "maas_request_rejections_total")
}

func TestNewMetricsServer_Insecure(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	srv, err := metrics.NewMetricsServer(metrics.ServerOptions{
		Addr:   ":0",
		Reg:    reg,
		Secure: false,
	})
	require.NoError(t, err)
	require.Nil(t, srv.TLSConfig)
}

func TestNewMetricsServer_SecureRequiresRESTConfig(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	_, err := metrics.NewMetricsServer(metrics.ServerOptions{
		Addr:    ":0",
		Reg:     reg,
		Secure:  true,
		CertDir: t.TempDir(),
	})
	require.Error(t, err)
}

func TestNewMetricsServer_SecureRequiresCertDir(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	_, err := metrics.NewMetricsServer(metrics.ServerOptions{
		Addr:       ":0",
		Reg:        reg,
		Secure:     true,
		RESTConfig: &rest.Config{Host: "https://example.invalid"},
	})
	require.Error(t, err)
}

func TestNewMetricsServer_SecureLoadsTLS(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestCertPair(t, dir)

	reg := prometheus.NewRegistry()
	restCfg := &rest.Config{Host: "https://127.0.0.1:1"}
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only
		},
		Timeout: 50 * time.Millisecond,
	}

	srv, err := metrics.NewMetricsServer(metrics.ServerOptions{
		Addr:       ":0",
		Reg:        reg,
		Secure:     true,
		CertDir:    dir,
		RESTConfig: restCfg,
		HTTPClient: httpClient,
	})
	require.NoError(t, err)
	require.NotNil(t, srv.TLSConfig)
	require.Equal(t, uint16(tls.VersionTLS12), srv.TLSConfig.MinVersion)
}

func writeTestCertPair(t *testing.T, dir string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.crt"), certPEM, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.key"), keyPEM, 0o600))
}
