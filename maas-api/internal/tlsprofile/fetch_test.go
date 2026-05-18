package tlsprofile_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/tlsprofile"
)

func newAPIServerObj(profile map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "config.openshift.io/v1",
		"kind":       "APIServer",
		"metadata":   map[string]any{"name": "cluster"},
	}}
	if profile != nil {
		obj.Object["spec"] = map[string]any{"tlsSecurityProfile": profile}
	}
	return obj
}

func TestParseProfileFromAPIServer_NoProfile(t *testing.T) {
	obj := newAPIServerObj(nil)
	spec, err := tlsprofile.ParseProfileFromAPIServer(obj)
	require.NoError(t, err)
	assert.Equal(t, tlsprofile.ProfileIntermediate, spec.Type)
}

func TestParseProfileFromAPIServer_EmptyType(t *testing.T) {
	obj := newAPIServerObj(map[string]any{"type": ""})
	spec, err := tlsprofile.ParseProfileFromAPIServer(obj)
	require.NoError(t, err)
	assert.Equal(t, tlsprofile.ProfileIntermediate, spec.Type)
}

func TestParseProfileFromAPIServer_NamedProfiles(t *testing.T) {
	tests := []struct {
		name     string
		profType string
		wantType tlsprofile.ProfileType
		wantMin  string
	}{
		{"Old", "Old", tlsprofile.ProfileOld, "VersionTLS10"},
		{"Intermediate", "Intermediate", tlsprofile.ProfileIntermediate, "VersionTLS12"},
		{"Modern", "Modern", tlsprofile.ProfileModern, "VersionTLS13"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := newAPIServerObj(map[string]any{"type": tt.profType})
			spec, err := tlsprofile.ParseProfileFromAPIServer(obj)
			require.NoError(t, err)
			assert.Equal(t, tt.wantType, spec.Type)
			assert.Equal(t, tt.wantMin, spec.MinTLSVersion)
		})
	}
}

func TestParseProfileFromAPIServer_NamedProfileReturnsClone(t *testing.T) {
	obj := newAPIServerObj(map[string]any{"type": "Intermediate"})
	p1, err := tlsprofile.ParseProfileFromAPIServer(obj)
	require.NoError(t, err)

	p2, err := tlsprofile.ParseProfileFromAPIServer(obj)
	require.NoError(t, err)

	p1.Ciphers[0] = "MUTATED"
	assert.NotEqual(t, p1.Ciphers[0], p2.Ciphers[0], "mutating one result must not affect another")
}

func TestParseProfileFromAPIServer_UnrecognizedType(t *testing.T) {
	obj := newAPIServerObj(map[string]any{"type": "SuperSecure"})
	spec, err := tlsprofile.ParseProfileFromAPIServer(obj)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unrecognized TLS profile type")
	assert.Equal(t, tlsprofile.ProfileIntermediate, spec.Type)
}

func TestParseProfileFromAPIServer_CustomProfile(t *testing.T) {
	obj := newAPIServerObj(map[string]any{
		"type": "Custom",
		"custom": map[string]any{
			"ciphers":       []any{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"},
			"minTLSVersion": "VersionTLS13",
		},
	})
	spec, err := tlsprofile.ParseProfileFromAPIServer(obj)
	require.NoError(t, err)
	assert.Equal(t, tlsprofile.ProfileCustom, spec.Type)
	assert.Equal(t, "VersionTLS13", spec.MinTLSVersion)
	assert.Equal(t, []string{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"}, spec.Ciphers)
}

func TestParseProfileFromAPIServer_CustomMissingCiphers(t *testing.T) {
	obj := newAPIServerObj(map[string]any{
		"type":   "Custom",
		"custom": map[string]any{"minTLSVersion": "VersionTLS12"},
	})
	_, err := tlsprofile.ParseProfileFromAPIServer(obj)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty cipher list")
}

func TestParseProfileFromAPIServer_CustomMissingSection(t *testing.T) {
	obj := newAPIServerObj(map[string]any{"type": "Custom"})
	_, err := tlsprofile.ParseProfileFromAPIServer(obj)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "custom is missing")
}

func TestParseProfileFromAPIServer_CustomInvalidMinVersion(t *testing.T) {
	obj := newAPIServerObj(map[string]any{
		"type": "Custom",
		"custom": map[string]any{
			"ciphers":       []any{"ECDHE-RSA-AES128-GCM-SHA256"},
			"minTLSVersion": "VersionTLS99",
		},
	})
	_, err := tlsprofile.ParseProfileFromAPIServer(obj)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unrecognized minTLSVersion")
}

func TestParseProfileFromAPIServer_CustomDefaultMinVersion(t *testing.T) {
	obj := newAPIServerObj(map[string]any{
		"type": "Custom",
		"custom": map[string]any{
			"ciphers": []any{"ECDHE-RSA-AES128-GCM-SHA256"},
		},
	})
	spec, err := tlsprofile.ParseProfileFromAPIServer(obj)
	require.NoError(t, err)
	assert.Equal(t, "VersionTLS12", spec.MinTLSVersion)
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
