package tlsprofile_test

import (
	"context"
	"errors"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/tlsprofile"
)

func newAPIServerObj(profile *configv1.TLSSecurityProfile) *configv1.APIServer {
	return &configv1.APIServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: configv1.APIServerSpec{
			TLSSecurityProfile: profile,
		},
	}
}

func TestParseProfileFromAPIServer_NoProfile(t *testing.T) {
	obj := newAPIServerObj(nil)
	spec, err := tlsprofile.ProfileFromAPIServer(obj)
	require.NoError(t, err)
	assert.Equal(t, tlsprofile.ProfileIntermediate, spec.Type)
}

func TestParseProfileFromAPIServer_EmptyType(t *testing.T) {
	obj := newAPIServerObj(&configv1.TLSSecurityProfile{})
	spec, err := tlsprofile.ProfileFromAPIServer(obj)
	require.NoError(t, err)
	assert.Equal(t, tlsprofile.ProfileIntermediate, spec.Type)
}

func TestParseProfileFromAPIServer_NamedProfiles(t *testing.T) {
	tests := []struct {
		name     string
		profType configv1.TLSProfileType
		wantType tlsprofile.ProfileType
		wantMin  configv1.TLSProtocolVersion
	}{
		{"Old", configv1.TLSProfileOldType, tlsprofile.ProfileOld, configv1.VersionTLS10},
		{"Intermediate", configv1.TLSProfileIntermediateType, tlsprofile.ProfileIntermediate, configv1.VersionTLS12},
		{"Modern", configv1.TLSProfileModernType, tlsprofile.ProfileModern, configv1.VersionTLS13},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := newAPIServerObj(&configv1.TLSSecurityProfile{Type: tt.profType})
			spec, err := tlsprofile.ProfileFromAPIServer(obj)
			require.NoError(t, err)
			assert.Equal(t, tt.wantType, spec.Type)
			assert.Equal(t, tt.wantMin, spec.MinTLSVersion)
		})
	}
}

func TestParseProfileFromAPIServer_NamedProfileReturnsClone(t *testing.T) {
	obj := newAPIServerObj(&configv1.TLSSecurityProfile{Type: configv1.TLSProfileIntermediateType})
	p1, err := tlsprofile.ProfileFromAPIServer(obj)
	require.NoError(t, err)

	p2, err := tlsprofile.ProfileFromAPIServer(obj)
	require.NoError(t, err)

	p1.Ciphers[0] = "MUTATED"
	assert.NotEqual(t, p1.Ciphers[0], p2.Ciphers[0], "mutating one result must not affect another")
}

func TestParseProfileFromAPIServer_UnrecognizedType(t *testing.T) {
	obj := newAPIServerObj(&configv1.TLSSecurityProfile{Type: configv1.TLSProfileType("SuperSecure")})
	spec, err := tlsprofile.ProfileFromAPIServer(obj)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unrecognized TLS profile type")
	assert.Equal(t, tlsprofile.ProfileIntermediate, spec.Type)
}

func TestParseProfileFromAPIServer_CustomProfile(t *testing.T) {
	obj := newAPIServerObj(&configv1.TLSSecurityProfile{
		Type: configv1.TLSProfileCustomType,
		Custom: &configv1.CustomTLSProfile{
			TLSProfileSpec: configv1.TLSProfileSpec{
				Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"},
				MinTLSVersion: configv1.VersionTLS13,
			},
		},
	})
	spec, err := tlsprofile.ProfileFromAPIServer(obj)
	require.NoError(t, err)
	assert.Equal(t, tlsprofile.ProfileCustom, spec.Type)
	assert.Equal(t, configv1.VersionTLS13, spec.MinTLSVersion)
	assert.Equal(t, []string{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"}, spec.Ciphers)
}

func TestParseProfileFromAPIServer_CustomMissingCiphers(t *testing.T) {
	obj := newAPIServerObj(&configv1.TLSSecurityProfile{
		Type: configv1.TLSProfileCustomType,
		Custom: &configv1.CustomTLSProfile{
			TLSProfileSpec: configv1.TLSProfileSpec{
				MinTLSVersion: configv1.VersionTLS12,
			},
		},
	})
	_, err := tlsprofile.ProfileFromAPIServer(obj)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty cipher list")
}

func TestParseProfileFromAPIServer_CustomMissingSection(t *testing.T) {
	obj := newAPIServerObj(&configv1.TLSSecurityProfile{Type: configv1.TLSProfileCustomType})
	_, err := tlsprofile.ProfileFromAPIServer(obj)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "custom is missing")
}

func TestParseProfileFromAPIServer_CustomInvalidMinVersion(t *testing.T) {
	obj := newAPIServerObj(&configv1.TLSSecurityProfile{
		Type: configv1.TLSProfileCustomType,
		Custom: &configv1.CustomTLSProfile{
			TLSProfileSpec: configv1.TLSProfileSpec{
				Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256"},
				MinTLSVersion: configv1.TLSProtocolVersion("VersionTLS99"),
			},
		},
	})
	_, err := tlsprofile.ProfileFromAPIServer(obj)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unrecognized minTLSVersion")
}

func TestParseProfileFromAPIServer_CustomDefaultMinVersion(t *testing.T) {
	obj := newAPIServerObj(&configv1.TLSSecurityProfile{
		Type: configv1.TLSProfileCustomType,
		Custom: &configv1.CustomTLSProfile{
			TLSProfileSpec: configv1.TLSProfileSpec{
				Ciphers: []string{"ECDHE-RSA-AES128-GCM-SHA256"},
			},
		},
	})
	spec, err := tlsprofile.ProfileFromAPIServer(obj)
	require.NoError(t, err)
	assert.Equal(t, configv1.VersionTLS12, spec.MinTLSVersion)
}

func TestIsAPIUnavailable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"generic error", errors.New("connection refused"), false},
		{
			"not found",
			apierrors.NewNotFound(schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"}, "cluster"),
			true,
		},
		{
			"server error",
			apierrors.NewInternalError(errors.New("apiserver down")),
			false,
		},
		{
			"forbidden",
			apierrors.NewForbidden(schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"}, "cluster", errors.New("denied")),
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tlsprofile.IsAPIUnavailable(tt.err))
		})
	}
}

func TestFetchTLSProfile_NilRestConfig(t *testing.T) {
	_, err := tlsprofile.FetchTLSProfile(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not be nil")
}

func TestNewWatcher_NilRestConfig(t *testing.T) {
	_, err := tlsprofile.NewWatcher(nil, tlsprofile.DefaultProfile(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not be nil")
}
