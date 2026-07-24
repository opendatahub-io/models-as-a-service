package main

import (
	"context"
	"crypto/tls"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/config"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/tlsprofile"
)

func TestBuildTLSConfig_ProfileOverridesFlag(t *testing.T) {
	cfg := &config.Config{
		TLS: config.TLSConfig{
			SelfSigned: true,
			MinVersion: config.TLSVersion(tls.VersionTLS12),
		},
		Name: "test-service",
	}

	profileMinVersion := uint16(tls.VersionTLS13)
	profileCipherSuites := []uint16{
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	}

	tlsCfg, err := buildTLSConfig(cfg, profileMinVersion, profileCipherSuites)
	require.NoError(t, err)

	assert.Equal(t, uint16(tls.VersionTLS13), tlsCfg.MinVersion,
		"profile minVersion should override flag-based default")
	assert.Equal(t, profileCipherSuites, tlsCfg.CipherSuites,
		"profile cipher suites should be applied")
	assert.Equal(t, []string{"h2", "http/1.1"}, tlsCfg.NextProtos)
}

func TestBuildTLSConfig_FlagDefaultWhenNoProfile(t *testing.T) {
	cfg := &config.Config{
		TLS: config.TLSConfig{
			SelfSigned: true,
			MinVersion: config.TLSVersion(tls.VersionTLS12),
		},
		Name: "test-service",
	}

	tlsCfg, err := buildTLSConfig(cfg, 0, nil)
	require.NoError(t, err)

	assert.Equal(t, uint16(tls.VersionTLS12), tlsCfg.MinVersion,
		"flag default should apply when profileMinVersion is 0")
	assert.Nil(t, tlsCfg.CipherSuites,
		"CipherSuites should be nil when no profile suites provided")
	assert.Equal(t, []string{"h2", "http/1.1"}, tlsCfg.NextProtos)
}

func TestBuildTLSConfig_ProfileCipherSuitesEmpty(t *testing.T) {
	cfg := &config.Config{
		TLS: config.TLSConfig{
			SelfSigned: true,
			MinVersion: config.TLSVersion(tls.VersionTLS12),
		},
		Name: "test-service",
	}

	profileMinVersion := uint16(tls.VersionTLS13)
	var emptyCiphers []uint16

	tlsCfg, err := buildTLSConfig(cfg, profileMinVersion, emptyCiphers)
	require.NoError(t, err)

	assert.Equal(t, uint16(tls.VersionTLS13), tlsCfg.MinVersion)
	assert.Nil(t, tlsCfg.CipherSuites,
		"CipherSuites should be nil when profile provides empty slice (Go defaults apply)")
	assert.Equal(t, []string{"h2", "http/1.1"}, tlsCfg.NextProtos)
}

func TestFetchTLSProfileWithRetry_TransientErrorFallsBackToIntermediate(t *testing.T) {
	originalFetch := fetchClusterTLSSettings
	originalDelay := tlsProfileRetryDelay
	defer func() {
		fetchClusterTLSSettings = originalFetch
		tlsProfileRetryDelay = originalDelay
	}()

	tlsProfileRetryDelay = 0
	calls := 0
	fetchClusterTLSSettings = func(context.Context, *rest.Config) (tlsprofile.Settings, error) {
		calls++
		return tlsprofile.DefaultSettings(), errors.New("temporary apiserver error")
	}

	settings, watchSettings, err := fetchTLSSettingsWithRetry(context.Background(), logger.New(false), &rest.Config{})
	require.NoError(t, err)

	assert.Equal(t, tlsProfileFetchMaxRetries, calls)
	assert.True(t, watchSettings, "OpenShift-like transient failures should still start the watcher")
	assert.Equal(t, tlsprofile.ProfileIntermediate, settings.Profile.Type)
}

func TestFetchTLSProfileWithRetry_APIUnavailableFallsBackAndSkipsWatcher(t *testing.T) {
	originalFetch := fetchClusterTLSSettings
	defer func() {
		fetchClusterTLSSettings = originalFetch
	}()

	fetchClusterTLSSettings = func(context.Context, *rest.Config) (tlsprofile.Settings, error) {
		return tlsprofile.DefaultSettings(), apierrors.NewNotFound(
			schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"},
			"cluster",
		)
	}

	settings, watchSettings, err := fetchTLSSettingsWithRetry(context.Background(), logger.New(false), &rest.Config{})
	require.NoError(t, err)

	assert.False(t, watchSettings, "non-OpenShift clusters should skip the config.openshift.io watcher")
	assert.Equal(t, tlsprofile.ProfileIntermediate, settings.Profile.Type)
}
