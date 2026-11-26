package metrics_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/cert"
	"github.com/opendatahub-io/models-as-a-service/maas-discovery/internal/metrics"
)

// fakeAuthzClient returns a clientset whose TokenReview authenticates the
// given user and whose SubjectAccessReview allows GET /metrics only when
// allow is true.
func fakeAuthzClient(username string, allow bool) *kubefake.Clientset {
	client := kubefake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.TokenReview{
			Status: authv1.TokenReviewStatus{
				Authenticated: username != "",
				User: authv1.UserInfo{
					Username: username,
					Groups:   []string{"system:authenticated"},
				},
			},
		}, nil
	})
	client.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authzv1.SubjectAccessReview{
			Status: authzv1.SubjectAccessReviewStatus{Allowed: allow},
		}, nil
	})
	return client
}

func writeTestCerts(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	tlsCert, err := cert.Generate("maas-discovery-metrics-test")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.crt"), encodePEMCert(tlsCert), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.key"), encodePEMKey(tlsCert), 0o600))
	return dir
}

func TestNewServer_SecureRequiresKubeClientAndCertDir(t *testing.T) {
	m := metrics.New()

	_, err := m.NewServer(metrics.ServerOptions{Addr: ":0", Secure: true})
	require.ErrorContains(t, err, "kubernetes client")

	_, err = m.NewServer(metrics.ServerOptions{Addr: ":0", Secure: true, KubeClient: fakeAuthzClient("scraper", true)})
	assert.ErrorContains(t, err, "cert dir")
}

func TestNewServer_SecureServesMetricsOverTLSWithAuthz(t *testing.T) {
	m := metrics.New()
	m.SetInformerSynced(metrics.ResourceAITenants, true)

	srv, err := m.NewServer(metrics.ServerOptions{
		Addr:       "127.0.0.1:0",
		Secure:     true,
		CertDir:    writeTestCerts(t),
		KubeClient: fakeAuthzClient("scraper", true),
	})
	require.NoError(t, err)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go srv.ServeTLS(ln, "", "") //nolint:errcheck // test server

	// The scrape must use the self-signed cert's CA pool.
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only, self-signed
	}}

	req, err := http.NewRequest(http.MethodGet, "https://"+ln.Addr().String()+"/metrics", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer scraper-token")

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "discovery_informer_synced")
}

func TestMetricsAuthFilter(t *testing.T) {
	m := metrics.New()

	cases := []struct {
		name       string
		client     *kubefake.Clientset
		authHeader string
		wantCode   int
	}{
		{
			name:       "no bearer token",
			client:     fakeAuthzClient("scraper", true),
			authHeader: "",
			wantCode:   http.StatusUnauthorized,
		},
		{
			name:       "token not authenticated",
			client:     fakeAuthzClient("", true),
			authHeader: "Bearer bad-token",
			wantCode:   http.StatusUnauthorized,
		},
		{
			name:       "authenticated but not authorized",
			client:     fakeAuthzClient("scraper", false),
			authHeader: "Bearer scraper-token",
			wantCode:   http.StatusForbidden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := m.NewServer(metrics.ServerOptions{
				Addr:       "127.0.0.1:0",
				Secure:     true,
				CertDir:    writeTestCerts(t),
				KubeClient: tc.client,
			})
			require.NoError(t, err)

			req, err := http.NewRequest(http.MethodGet, "https://127.0.0.1/metrics", nil)
			require.NoError(t, err)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}

			// Route directly through the server handler to avoid port
			// allocation for each subtest.
			w := httptest.NewRecorder()
			srv.Handler.ServeHTTP(w, req)
			assert.Equal(t, tc.wantCode, w.Code)
		})
	}
}

func TestMetricsAuthFilter_FailsClosedOnAPIError(t *testing.T) {
	m := metrics.New()

	client := kubefake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, assert.AnError
	})

	srv, err := m.NewServer(metrics.ServerOptions{
		Addr:       "127.0.0.1:0",
		Secure:     true,
		CertDir:    writeTestCerts(t),
		KubeClient: client,
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer some-token")
	w := httptest.NewRecorder()
	srv.Handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestMetricsAuthFilter_AuthorizesNonResourceURLGet(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	var sarSeen *authzv1.SubjectAccessReview
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.TokenReview{
			Status: authv1.TokenReviewStatus{
				Authenticated: true,
				User:          authv1.UserInfo{Username: "scraper"},
			},
		}, nil
	})
	client.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj, ok := action.(k8stesting.CreateAction).GetObject().(*authzv1.SubjectAccessReview)
		if ok {
			sarSeen = obj
		}
		return true, &authzv1.SubjectAccessReview{
			Status: authzv1.SubjectAccessReviewStatus{Allowed: true},
		}, nil
	})

	m := metrics.New()
	srv, err := m.NewServer(metrics.ServerOptions{
		Addr:       "127.0.0.1:0",
		Secure:     true,
		CertDir:    writeTestCerts(t),
		KubeClient: client,
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer scraper-token")
	w := httptest.NewRecorder()
	srv.Handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	require.NotNil(t, sarSeen)
	nra := sarSeen.Spec.NonResourceAttributes
	require.NotNil(t, nra)
	assert.Equal(t, "/metrics", nra.Path)
	assert.Equal(t, "get", nra.Verb)
	assert.Equal(t, "scraper", sarSeen.Spec.User)
}

// encodePEMCert/encodePEMKey serialize the in-memory certificate to the PEM
// bytes the cert loader expects on disk.
func encodePEMCert(c tls.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[0]})
}

func encodePEMKey(c tls.Certificate) []byte {
	der, err := x509.MarshalPKCS8PrivateKey(c.PrivateKey)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}
