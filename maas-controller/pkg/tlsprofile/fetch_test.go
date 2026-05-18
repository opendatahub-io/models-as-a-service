package tlsprofile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	spec, err := parseProfileFromAPIServer(obj)
	require.NoError(t, err)
	assert.Equal(t, ProfileIntermediate, spec.Type)
}

func TestParseProfileFromAPIServer_EmptyType(t *testing.T) {
	obj := newAPIServerObj(map[string]any{"type": ""})
	spec, err := parseProfileFromAPIServer(obj)
	require.NoError(t, err)
	assert.Equal(t, ProfileIntermediate, spec.Type)
}

func TestParseProfileFromAPIServer_NamedProfiles(t *testing.T) {
	tests := []struct {
		name     string
		profType string
		wantType ProfileType
		wantMin  string
	}{
		{"Old", "Old", ProfileOld, "VersionTLS10"},
		{"Intermediate", "Intermediate", ProfileIntermediate, "VersionTLS12"},
		{"Modern", "Modern", ProfileModern, "VersionTLS13"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := newAPIServerObj(map[string]any{"type": tt.profType})
			spec, err := parseProfileFromAPIServer(obj)
			require.NoError(t, err)
			assert.Equal(t, tt.wantType, spec.Type)
			assert.Equal(t, tt.wantMin, spec.MinTLSVersion)
		})
	}
}

func TestParseProfileFromAPIServer_UnrecognizedType(t *testing.T) {
	obj := newAPIServerObj(map[string]any{"type": "SuperSecure"})
	spec, err := parseProfileFromAPIServer(obj)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unrecognized TLS profile type")
	assert.Equal(t, ProfileIntermediate, spec.Type)
}

func TestParseProfileFromAPIServer_CustomProfile(t *testing.T) {
	obj := newAPIServerObj(map[string]any{
		"type": "Custom",
		"custom": map[string]any{
			"ciphers":       []any{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"},
			"minTLSVersion": "VersionTLS13",
		},
	})
	spec, err := parseProfileFromAPIServer(obj)
	require.NoError(t, err)
	assert.Equal(t, ProfileCustom, spec.Type)
	assert.Equal(t, "VersionTLS13", spec.MinTLSVersion)
	assert.Equal(t, []string{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"}, spec.Ciphers)
}

func TestParseProfileFromAPIServer_CustomMissingCiphers(t *testing.T) {
	obj := newAPIServerObj(map[string]any{
		"type":   "Custom",
		"custom": map[string]any{"minTLSVersion": "VersionTLS12"},
	})
	_, err := parseProfileFromAPIServer(obj)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty cipher list")
}

func TestParseProfileFromAPIServer_CustomMissingSection(t *testing.T) {
	obj := newAPIServerObj(map[string]any{"type": "Custom"})
	_, err := parseProfileFromAPIServer(obj)
	assert.Error(t, err)
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
	_, err := parseProfileFromAPIServer(obj)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unrecognized minTLSVersion")
}

func TestParseProfileFromAPIServer_CustomDefaultMinVersion(t *testing.T) {
	obj := newAPIServerObj(map[string]any{
		"type": "Custom",
		"custom": map[string]any{
			"ciphers": []any{"ECDHE-RSA-AES128-GCM-SHA256"},
		},
	})
	spec, err := parseProfileFromAPIServer(obj)
	require.NoError(t, err)
	assert.Equal(t, "VersionTLS12", spec.MinTLSVersion)
}
