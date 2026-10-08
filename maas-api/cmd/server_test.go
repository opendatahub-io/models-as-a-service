package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
	originalAvailable := configAPIAvailable
	originalDelay := tlsProfileRetryDelay
	defer func() {
		fetchClusterTLSSettings = originalFetch
		configAPIAvailable = originalAvailable
		tlsProfileRetryDelay = originalDelay
	}()

	tlsProfileRetryDelay = 0
	configAPIAvailable = func(context.Context, *rest.Config) (bool, error) { return true, nil }
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
	originalAvailable := configAPIAvailable
	defer func() {
		fetchClusterTLSSettings = originalFetch
		configAPIAvailable = originalAvailable
	}()

	configAPIAvailable = func(context.Context, *rest.Config) (bool, error) { return true, nil }
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

func TestFetchTLSProfileWithRetry_DiscoverySkipsProfileRequests(t *testing.T) {
	originalFetch := fetchClusterTLSSettings
	originalAvailable := configAPIAvailable
	defer func() {
		fetchClusterTLSSettings = originalFetch
		configAPIAvailable = originalAvailable
	}()

	configAPIAvailable = func(context.Context, *rest.Config) (bool, error) { return false, nil }
	fetchClusterTLSSettings = func(context.Context, *rest.Config) (tlsprofile.Settings, error) {
		t.Error("profile must not be requested when discovery shows config.openshift.io is not served")
		return tlsprofile.DefaultSettings(), errors.New("unexpected fetch")
	}

	settings, watchSettings, err := fetchTLSSettingsWithRetry(context.Background(), logger.New(false), &rest.Config{})
	require.NoError(t, err)

	assert.False(t, watchSettings, "non-OpenShift clusters should skip the config.openshift.io watcher")
	assert.Equal(t, tlsprofile.ProfileIntermediate, settings.Profile.Type)
}

func TestFetchTLSProfileWithRetry_ForbiddenSurfacesAuthorizationError(t *testing.T) {
	originalFetch := fetchClusterTLSSettings
	originalAvailable := configAPIAvailable
	defer func() {
		fetchClusterTLSSettings = originalFetch
		configAPIAvailable = originalAvailable
	}()

	configAPIAvailable = func(context.Context, *rest.Config) (bool, error) { return true, nil }
	calls := 0
	fetchClusterTLSSettings = func(context.Context, *rest.Config) (tlsprofile.Settings, error) {
		calls++
		return tlsprofile.DefaultSettings(), apierrors.NewForbidden(
			schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"},
			"cluster", errors.New("not authorized"))
	}

	settings, watchSettings, err := fetchTLSSettingsWithRetry(context.Background(), logger.New(false), &rest.Config{})

	require.Error(t, err, "authorization errors against a served config.openshift.io must be preserved")
	require.ErrorContains(t, err, "forbidden")
	assert.Equal(t, 1, calls, "authorization errors must not be retried into a watcher that cannot sync")
	assert.False(t, watchSettings)
	assert.Equal(t, tlsprofile.ProfileIntermediate, settings.Profile.Type)
}

func TestFetchTLSProfileWithRetry_DiscoveryFailureFallsBackToProbe(t *testing.T) {
	originalFetch := fetchClusterTLSSettings
	originalAvailable := configAPIAvailable
	defer func() {
		fetchClusterTLSSettings = originalFetch
		configAPIAvailable = originalAvailable
	}()

	configAPIAvailable = func(context.Context, *rest.Config) (bool, error) {
		return false, errors.New("apiserver unreachable")
	}
	fetchClusterTLSSettings = func(context.Context, *rest.Config) (tlsprofile.Settings, error) {
		return tlsprofile.DefaultSettings(), apierrors.NewNotFound(
			schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"},
			"cluster",
		)
	}

	settings, watchSettings, err := fetchTLSSettingsWithRetry(context.Background(), logger.New(false), &rest.Config{})
	require.NoError(t, err)

	assert.False(t, watchSettings, "probe-recognized unavailability must still skip the watcher")
	assert.Equal(t, tlsprofile.ProfileIntermediate, settings.Profile.Type)
}

// On xKS an unserved config.openshift.io answers 403 (authorization runs before
// routing), so when discovery fails a forbidden probe must not be reported as
// missing RBAC on a served group: both causes are surfaced and nothing is retried.
func TestFetchTLSProfileWithRetry_DiscoveryFailureThenForbiddenReportsUnknownPlatform(t *testing.T) {
	originalFetch := fetchClusterTLSSettings
	originalAvailable := configAPIAvailable
	defer func() {
		fetchClusterTLSSettings = originalFetch
		configAPIAvailable = originalAvailable
	}()

	discoveryErr := errors.New("apiserver unreachable")
	configAPIAvailable = func(context.Context, *rest.Config) (bool, error) { return false, discoveryErr }
	calls := 0
	fetchClusterTLSSettings = func(context.Context, *rest.Config) (tlsprofile.Settings, error) {
		calls++
		return tlsprofile.DefaultSettings(), apierrors.NewForbidden(
			schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"},
			"cluster", errors.New("not authorized"))
	}

	settings, watchSettings, err := fetchTLSSettingsWithRetry(context.Background(), logger.New(false), &rest.Config{})

	require.Error(t, err)
	require.ErrorIs(t, err, discoveryErr, "the discovery failure must be surfaced")
	assert.True(t, apierrors.IsForbidden(err), "the authorization error must be preserved")
	assert.Equal(t, 1, calls)
	assert.False(t, watchSettings)
	assert.Equal(t, tlsprofile.ProfileIntermediate, settings.Profile.Type)
}

// TestFetchTLSProfileWithRetry_XKSRestrictedPermissions covers maas-api startup
// on a non-OpenShift cluster where config.openshift.io is not served and any
// request for it is forbidden (the service account holds no config.openshift.io
// RBAC). The real discovery client runs against a fake apiserver; the group list
// contains an unrelated OpenShift-ish group to prove the exact group-name match,
// and the config.openshift.io handler counts (and forbids) every request.
// Startup must succeed with the default profile, skip the watcher, and never
// request the profile.
func TestFetchTLSProfileWithRetry_XKSRestrictedPermissions(t *testing.T) {
	originalFetch := fetchClusterTLSSettings
	defer func() { fetchClusterTLSSettings = originalFetch }()

	profileRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"kind":"APIVersions","apiVersion":"v1","versions":["v1"]}`))
		case r.URL.Path == "/apis":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"kind":"APIGroupList","apiVersion":"v1","groups":[` +
				`{"name":"operator.openshift.io","versions":[{"groupVersion":"operator.openshift.io/v1","version":"v1"}],` +
				`"preferredVersion":{"groupVersion":"operator.openshift.io/v1","version":"v1"}}]}`))
		case strings.HasPrefix(r.URL.Path, "/apis/config.openshift.io"):
			profileRequests++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure",` +
				`"message":"forbidden: User \"system:serviceaccount:test:maas-api\" cannot get resource ` +
				`\"apiservers\" in API group \"config.openshift.io\"","reason":"Forbidden","details":{},"code":403}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	fetchClusterTLSSettings = func(context.Context, *rest.Config) (tlsprofile.Settings, error) {
		t.Error("profile must not be requested when discovery shows config.openshift.io is not served")
		return tlsprofile.DefaultSettings(), errors.New("unexpected fetch")
	}

	restConfig := &rest.Config{Host: server.URL}
	settings, watchSettings, err := fetchTLSSettingsWithRetry(context.Background(), logger.New(false), restConfig)
	require.NoError(t, err, "startup must succeed on a cluster that does not serve config.openshift.io")

	assert.False(t, watchSettings, "non-OpenShift clusters should skip the config.openshift.io watcher")
	assert.Equal(t, tlsprofile.ProfileIntermediate, settings.Profile.Type)
	assert.Zero(t, profileRequests,
		"no config.openshift.io request may be made when discovery shows the group is not served")
}

type stubTLSProfileWatcher struct {
	startErr error
}

func (s stubTLSProfileWatcher) Start(<-chan struct{}) error {
	return s.startErr
}

func TestSetupTLSProfile_SkipsWhenNeitherAPINorMetricsSecure(t *testing.T) {
	minVersion, ciphers, err := setupTLSProfile(t.Context(), logger.New(false), &config.Config{Secure: false, MetricsSecure: false}, &rest.Config{}, func() {})
	require.NoError(t, err)
	assert.Zero(t, minVersion)
	assert.Nil(t, ciphers)
}

func TestSetupTLSProfile_FailsWhenRESTConfigNilWithHTTPSEnabled(t *testing.T) {
	minVersion, ciphers, err := setupTLSProfile(t.Context(), logger.New(false), &config.Config{Secure: true}, nil, func() {})
	require.Error(t, err)
	assert.Zero(t, minVersion)
	assert.Nil(t, ciphers)
	assert.Contains(t, err.Error(), "REST configuration is unavailable")
}

func TestSetupTLSProfile_AppliesWhenMetricsSecureOnly(t *testing.T) {
	originalFetch := fetchClusterTLSSettings
	originalNew := newTLSProfileWatcher
	defer func() {
		fetchClusterTLSSettings = originalFetch
		newTLSProfileWatcher = originalNew
	}()

	fetchClusterTLSSettings = func(context.Context, *rest.Config) (tlsprofile.Settings, error) {
		return tlsprofile.DefaultSettings(), nil
	}
	created := false
	newTLSProfileWatcher = func(*rest.Config, tlsprofile.Settings, func(tlsprofile.Settings, tlsprofile.Settings)) (tlsProfileWatcher, error) {
		created = true
		return stubTLSProfileWatcher{}, nil
	}

	_, _, err := setupTLSProfile(t.Context(), logger.New(false), &config.Config{Secure: false, MetricsSecure: true}, &rest.Config{}, func() {})
	require.NoError(t, err)
	assert.True(t, created, "metrics HTTPS still requires a cluster TLS profile watcher")
}

func TestSetupTLSProfile_SkipsWatcherWhenAPIUnavailable(t *testing.T) {
	originalFetch := fetchClusterTLSSettings
	originalNew := newTLSProfileWatcher
	defer func() {
		fetchClusterTLSSettings = originalFetch
		newTLSProfileWatcher = originalNew
	}()

	fetchClusterTLSSettings = func(context.Context, *rest.Config) (tlsprofile.Settings, error) {
		return tlsprofile.DefaultSettings(), apierrors.NewNotFound(
			schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"},
			"cluster",
		)
	}
	newTLSProfileWatcher = func(*rest.Config, tlsprofile.Settings, func(tlsprofile.Settings, tlsprofile.Settings)) (tlsProfileWatcher, error) {
		t.Fatal("watcher should not be created on non-OpenShift clusters")
		return nil, nil
	}

	_, _, err := setupTLSProfile(t.Context(), logger.New(false), &config.Config{Secure: true}, &rest.Config{}, func() {})
	require.NoError(t, err)
}

func TestSetupTLSProfile_WatcherCreateFailsClosed(t *testing.T) {
	originalFetch := fetchClusterTLSSettings
	originalNew := newTLSProfileWatcher
	defer func() {
		fetchClusterTLSSettings = originalFetch
		newTLSProfileWatcher = originalNew
	}()

	fetchClusterTLSSettings = func(context.Context, *rest.Config) (tlsprofile.Settings, error) {
		return tlsprofile.DefaultSettings(), nil
	}
	newTLSProfileWatcher = func(*rest.Config, tlsprofile.Settings, func(tlsprofile.Settings, tlsprofile.Settings)) (tlsProfileWatcher, error) {
		return nil, errors.New("restConfig rejected")
	}

	_, _, err := setupTLSProfile(t.Context(), logger.New(false), &config.Config{Secure: true}, &rest.Config{}, func() {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unable to create TLS profile watcher")
}

func TestSetupTLSProfile_WatcherSyncFailsClosed(t *testing.T) {
	originalFetch := fetchClusterTLSSettings
	originalNew := newTLSProfileWatcher
	defer func() {
		fetchClusterTLSSettings = originalFetch
		newTLSProfileWatcher = originalNew
	}()

	fetchClusterTLSSettings = func(context.Context, *rest.Config) (tlsprofile.Settings, error) {
		return tlsprofile.DefaultSettings(), nil
	}
	newTLSProfileWatcher = func(*rest.Config, tlsprofile.Settings, func(tlsprofile.Settings, tlsprofile.Settings)) (tlsProfileWatcher, error) {
		return stubTLSProfileWatcher{startErr: errors.New("informer cache sync failed")}, nil
	}

	_, _, err := setupTLSProfile(t.Context(), logger.New(false), &config.Config{Secure: true}, &rest.Config{}, func() {
		t.Fatal("cancel should not be called; Start errors must fail setupTLSProfile")
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TLS profile watcher failed to sync")
}

func TestSetupTLSProfile_WatcherStartSucceeds(t *testing.T) {
	originalFetch := fetchClusterTLSSettings
	originalNew := newTLSProfileWatcher
	defer func() {
		fetchClusterTLSSettings = originalFetch
		newTLSProfileWatcher = originalNew
	}()

	fetchClusterTLSSettings = func(context.Context, *rest.Config) (tlsprofile.Settings, error) {
		return tlsprofile.DefaultSettings(), nil
	}
	newTLSProfileWatcher = func(*rest.Config, tlsprofile.Settings, func(tlsprofile.Settings, tlsprofile.Settings)) (tlsProfileWatcher, error) {
		return stubTLSProfileWatcher{}, nil
	}

	cancelled := false
	_, _, err := setupTLSProfile(t.Context(), logger.New(false), &config.Config{Secure: true}, &rest.Config{}, func() {
		cancelled = true
	})
	require.NoError(t, err)
	assert.False(t, cancelled)
}
