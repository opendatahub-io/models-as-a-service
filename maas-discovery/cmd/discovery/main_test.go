package main

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
)

func TestFetchTLSSettingsWithRetryAuthorizationErrorsFailImmediately(t *testing.T) {
	tests := []struct {
		name   string
		status int
		reason string
	}{
		{"forbidden", http.StatusForbidden, "Forbidden"},
		{"unauthorized", http.StatusUnauthorized, "Unauthorized"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","metadata":{},` +
					`"status":"Failure","message":"access denied","reason":"` + tt.reason +
					`","code":` + strconv.Itoa(tt.status) + `}`))
			}))
			defer server.Close()

			settings, watchSettings, err := fetchTLSSettingsWithRetry(
				context.Background(), testLogger(), &rest.Config{Host: server.URL})

			require.Error(t, err)
			assert.Equal(t, 1, requests)
			assert.False(t, watchSettings)
			assert.Equal(t, "Intermediate", string(settings.Profile.Type))
			if tt.status == http.StatusForbidden {
				assert.True(t, apierrors.IsForbidden(err))
			} else {
				assert.True(t, apierrors.IsUnauthorized(err))
			}
		})
	}
}

func TestFetchTLSSettingsWithRetryInvalidProfileFailsImmediately(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"APIServer","apiVersion":"config.openshift.io/v1",` +
			`"metadata":{"name":"cluster"},"spec":{"tlsSecurityProfile":{"type":"Custom",` +
			`"custom":{"ciphers":["ECDHE-RSA-AES128-GCM-SHA256"],` +
			`"minTLSVersion":"VersionTLS99"}}}}`))
	}))
	defer server.Close()

	settings, watchSettings, err := fetchTLSSettingsWithRetry(
		context.Background(), testLogger(), &rest.Config{Host: server.URL})

	require.ErrorContains(t, err, "invalid cluster TLS profile")
	assert.Equal(t, 1, requests)
	assert.False(t, watchSettings)
	assert.Equal(t, "Intermediate", string(settings.Profile.Type))
}

func TestBuildTLSConfigHTTP2RequiresTLS12AndAES128GCM(t *testing.T) {
	tests := []struct {
		name       string
		minVersion uint16
		ciphers    []uint16
		want       []string
	}{
		{"TLS 1.2 with AES-128-GCM", tls.VersionTLS12,
			[]uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}, []string{"h2", "http/1.1"}},
		{"TLS 1.2 without AES-128-GCM", tls.VersionTLS12,
			[]uint16{tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384}, []string{"http/1.1"}},
		{"TLS 1.1 with AES-128-GCM", tls.VersionTLS11,
			[]uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}, []string{"http/1.1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := buildTLSConfig("", "", true, tt.minVersion, tt.ciphers)
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.NextProtos)
		})
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
