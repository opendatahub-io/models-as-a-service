package tlsprofile_test

import (
	"context"
	"errors"
	"testing"

	confv1 "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/tlsprofile"
)

func newAPIServerObj(profile *confv1.TLSSecurityProfile) *confv1.APIServer {
	return &confv1.APIServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: confv1.APIServerSpec{
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

func TestParseSettingsFromAPIServer_IncludesAdherence(t *testing.T) {
	obj := newAPIServerObj(&confv1.TLSSecurityProfile{Type: confv1.TLSProfileModernType})
	obj.Spec.TLSAdherence = confv1.TLSAdherencePolicyStrictAllComponents

	settings, err := tlsprofile.SettingsFromAPIServer(obj)

	require.NoError(t, err)
	assert.Equal(t, tlsprofile.ProfileModern, settings.Profile.Type)
	assert.Equal(t, confv1.TLSAdherencePolicyStrictAllComponents, settings.Adherence)
}

func TestParseProfileFromAPIServer_EmptyType(t *testing.T) {
	obj := newAPIServerObj(&confv1.TLSSecurityProfile{})
	spec, err := tlsprofile.ProfileFromAPIServer(obj)
	require.NoError(t, err)
	assert.Equal(t, tlsprofile.ProfileIntermediate, spec.Type)
}

func TestParseProfileFromAPIServer_NamedProfiles(t *testing.T) {
	tests := []struct {
		name     string
		profType confv1.TLSProfileType
		wantType tlsprofile.ProfileType
		wantMin  confv1.TLSProtocolVersion
	}{
		{"Old", confv1.TLSProfileOldType, tlsprofile.ProfileOld, confv1.VersionTLS10},
		{"Intermediate", confv1.TLSProfileIntermediateType, tlsprofile.ProfileIntermediate, confv1.VersionTLS12},
		{"Modern", confv1.TLSProfileModernType, tlsprofile.ProfileModern, confv1.VersionTLS13},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := newAPIServerObj(&confv1.TLSSecurityProfile{Type: tt.profType})
			spec, err := tlsprofile.ProfileFromAPIServer(obj)
			require.NoError(t, err)
			assert.Equal(t, tt.wantType, spec.Type)
			assert.Equal(t, tt.wantMin, spec.MinTLSVersion)
		})
	}
}

func TestParseProfileFromAPIServer_NamedProfileReturnsClone(t *testing.T) {
	obj := newAPIServerObj(&confv1.TLSSecurityProfile{Type: confv1.TLSProfileIntermediateType})
	p1, err := tlsprofile.ProfileFromAPIServer(obj)
	require.NoError(t, err)

	p2, err := tlsprofile.ProfileFromAPIServer(obj)
	require.NoError(t, err)

	p1.Ciphers[0] = "MUTATED"
	assert.NotEqual(t, p1.Ciphers[0], p2.Ciphers[0], "mutating one result must not affect another")
}

func TestParseProfileFromAPIServer_UnrecognizedType(t *testing.T) {
	obj := newAPIServerObj(&confv1.TLSSecurityProfile{Type: confv1.TLSProfileType("SuperSecure")})
	spec, err := tlsprofile.ProfileFromAPIServer(obj)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unrecognized TLS profile type")
	assert.Equal(t, tlsprofile.ProfileIntermediate, spec.Type)
}

func TestParseProfileFromAPIServer_CustomProfile(t *testing.T) {
	obj := newAPIServerObj(&confv1.TLSSecurityProfile{
		Type: confv1.TLSProfileCustomType,
		Custom: &confv1.CustomTLSProfile{
			TLSProfileSpec: confv1.TLSProfileSpec{
				Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"},
				MinTLSVersion: confv1.VersionTLS13,
			},
		},
	})
	spec, err := tlsprofile.ProfileFromAPIServer(obj)
	require.NoError(t, err)
	assert.Equal(t, tlsprofile.ProfileCustom, spec.Type)
	assert.Equal(t, confv1.VersionTLS13, spec.MinTLSVersion)
	assert.Equal(t, []string{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"}, spec.Ciphers)
}

func TestParseProfileFromAPIServer_CustomMissingCiphers(t *testing.T) {
	obj := newAPIServerObj(&confv1.TLSSecurityProfile{
		Type: confv1.TLSProfileCustomType,
		Custom: &confv1.CustomTLSProfile{
			TLSProfileSpec: confv1.TLSProfileSpec{
				MinTLSVersion: confv1.VersionTLS12,
			},
		},
	})
	_, err := tlsprofile.ProfileFromAPIServer(obj)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty cipher list")
}

func TestParseProfileFromAPIServer_CustomMissingSection(t *testing.T) {
	obj := newAPIServerObj(&confv1.TLSSecurityProfile{Type: confv1.TLSProfileCustomType})
	_, err := tlsprofile.ProfileFromAPIServer(obj)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "custom is missing")
}

func TestParseProfileFromAPIServer_CustomInvalidMinVersion(t *testing.T) {
	obj := newAPIServerObj(&confv1.TLSSecurityProfile{
		Type: confv1.TLSProfileCustomType,
		Custom: &confv1.CustomTLSProfile{
			TLSProfileSpec: confv1.TLSProfileSpec{
				Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256"},
				MinTLSVersion: confv1.TLSProtocolVersion("VersionTLS99"),
			},
		},
	})
	_, err := tlsprofile.ProfileFromAPIServer(obj)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unrecognized minTLSVersion")
}

func TestParseProfileFromAPIServer_CustomDefaultMinVersion(t *testing.T) {
	obj := newAPIServerObj(&confv1.TLSSecurityProfile{
		Type: confv1.TLSProfileCustomType,
		Custom: &confv1.CustomTLSProfile{
			TLSProfileSpec: confv1.TLSProfileSpec{
				Ciphers: []string{"ECDHE-RSA-AES128-GCM-SHA256"},
			},
		},
	})
	spec, err := tlsprofile.ProfileFromAPIServer(obj)
	require.NoError(t, err)
	assert.Equal(t, confv1.VersionTLS12, spec.MinTLSVersion)
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
			"no resource match",
			&apimeta.NoResourceMatchError{
				PartialResource: schema.GroupVersionResource{
					Group:    "config.openshift.io",
					Version:  "v1",
					Resource: "apiservers",
				},
			},
			true,
		},
		{
			"no kind match",
			&apimeta.NoKindMatchError{
				GroupKind:        schema.GroupKind{Group: "config.openshift.io", Kind: "APIServer"},
				SearchedVersions: []string{"v1"},
			},
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

func TestFetchTLSSettings_NilRestConfig(t *testing.T) {
	_, err := tlsprofile.FetchTLSSettings(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not be nil")
}

func TestNewWatcher_NilRestConfig(t *testing.T) {
	_, err := tlsprofile.NewWatcher(nil, tlsprofile.DefaultSettings(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not be nil")
}
