package tlsprofile_test

import (
	"crypto/tls"
	"testing"

	confv1 "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/tlsprofile"
)

func TestTLSConfigFromProfile_Intermediate(t *testing.T) {
	spec := tlsprofile.ProfileSpec{
		Type: tlsprofile.ProfileIntermediate,
		TLSProfileSpec: confv1.TLSProfileSpec{
			Ciphers: []string{
				"TLS_AES_128_GCM_SHA256",
				"TLS_AES_256_GCM_SHA384",
				"ECDHE-ECDSA-AES128-GCM-SHA256",
				"ECDHE-RSA-AES128-GCM-SHA256",
			},
			MinTLSVersion: confv1.VersionTLS12,
		},
	}

	minVer, ciphers, unsupported, err := tlsprofile.TLSConfigFromProfile(spec)

	require.NoError(t, err)
	assert.Equal(t, uint16(tls.VersionTLS12), minVer)
	assert.Contains(t, ciphers, tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256)
	assert.Contains(t, ciphers, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256)
	assert.Equal(t, []string{"TLS_AES_128_GCM_SHA256", "TLS_AES_256_GCM_SHA384"}, unsupported)
}

func TestTLSConfigFromProfile_TLS13CiphersSkipped(t *testing.T) {
	spec := tlsprofile.ProfileSpec{
		Type: tlsprofile.ProfileModern,
		TLSProfileSpec: confv1.TLSProfileSpec{
			Ciphers: []string{
				"TLS_AES_128_GCM_SHA256",
				"TLS_AES_256_GCM_SHA384",
				"TLS_CHACHA20_POLY1305_SHA256",
			},
			MinTLSVersion: confv1.VersionTLS13,
		},
	}

	minVer, ciphers, unsupported, err := tlsprofile.TLSConfigFromProfile(spec)

	require.NoError(t, err)
	assert.Equal(t, uint16(tls.VersionTLS13), minVer)
	assert.Empty(t, ciphers, "TLS 1.3 ciphers should be skipped (Go manages them)")
	assert.Empty(t, unsupported)
}

func TestTLSConfigFromProfile_UnsupportedCiphers(t *testing.T) {
	spec := tlsprofile.ProfileSpec{
		Type: tlsprofile.ProfileCustom,
		TLSProfileSpec: confv1.TLSProfileSpec{
			Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256", "UNKNOWN-CIPHER-XYZ"},
			MinTLSVersion: confv1.VersionTLS12,
		},
	}

	_, ciphers, unsupported, err := tlsprofile.TLSConfigFromProfile(spec)

	require.NoError(t, err)
	assert.Len(t, ciphers, 1)
	assert.Equal(t, []string{"UNKNOWN-CIPHER-XYZ"}, unsupported)
}

func TestTLSConfigFromProfile_InvalidMinVersion(t *testing.T) {
	spec := tlsprofile.ProfileSpec{
		Type: tlsprofile.ProfileCustom,
		TLSProfileSpec: confv1.TLSProfileSpec{
			Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256"},
			MinTLSVersion: confv1.TLSProtocolVersion("InvalidVersion"),
		},
	}

	gotMinVer, _, _, err := tlsprofile.TLSConfigFromProfile(spec)
	_ = gotMinVer

	require.ErrorContains(t, err, "unrecognized minimum TLS version")
}

func TestTLSConfigFromProfile_AllVersions(t *testing.T) {
	tests := []struct {
		version confv1.TLSProtocolVersion
		want    uint16
	}{
		{confv1.VersionTLS10, tls.VersionTLS10},
		{confv1.VersionTLS11, tls.VersionTLS11},
		{confv1.VersionTLS12, tls.VersionTLS12},
		{confv1.VersionTLS13, tls.VersionTLS13},
	}

	for _, tt := range tests {
		t.Run(string(tt.version), func(t *testing.T) {
			spec := tlsprofile.ProfileSpec{
				Type: tlsprofile.ProfileCustom,
				TLSProfileSpec: confv1.TLSProfileSpec{
					Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256"},
					MinTLSVersion: tt.version,
				},
			}
			minVer, _, _, err := tlsprofile.TLSConfigFromProfile(spec)
			require.NoError(t, err)
			assert.Equal(t, tt.want, minVer)
		})
	}
}

func TestTLSConfigFromProfile_CipherMapping(t *testing.T) {
	tests := []struct {
		openSSL string
		goID    uint16
	}{
		{"ECDHE-ECDSA-AES128-GCM-SHA256", tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		{"ECDHE-RSA-AES256-GCM-SHA384", tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384},
		{"ECDHE-ECDSA-CHACHA20-POLY1305", tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256},
		{"AES128-GCM-SHA256", tls.TLS_RSA_WITH_AES_128_GCM_SHA256},
		{"DES-CBC3-SHA", tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA},
	}

	for _, tt := range tests {
		t.Run(tt.openSSL, func(t *testing.T) {
			spec := tlsprofile.ProfileSpec{
				Type: tlsprofile.ProfileCustom,
				TLSProfileSpec: confv1.TLSProfileSpec{
					Ciphers:       []string{tt.openSSL},
					MinTLSVersion: confv1.VersionTLS12,
				},
			}
			_, ciphers, unsupported, err := tlsprofile.TLSConfigFromProfile(spec)
			require.NoError(t, err)
			assert.Empty(t, unsupported)
			assert.Equal(t, []uint16{tt.goID}, ciphers)
		})
	}
}

func TestTLSConfigFromProfile_IANACipherMapping(t *testing.T) {
	spec := tlsprofile.ProfileSpec{
		Type: tlsprofile.ProfileCustom,
		TLSProfileSpec: confv1.TLSProfileSpec{
			Ciphers: []string{
				"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
				"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
			},
			MinTLSVersion: confv1.VersionTLS12,
		},
	}

	_, ciphers, unsupported, err := tlsprofile.TLSConfigFromProfile(spec)

	require.NoError(t, err)
	assert.Empty(t, unsupported)
	assert.Equal(t, []uint16{
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	}, ciphers)
}

func TestTLSConfigFromProfile_DeduplicatesCipherAliases(t *testing.T) {
	spec := tlsprofile.ProfileSpec{
		Type: tlsprofile.ProfileCustom,
		TLSProfileSpec: confv1.TLSProfileSpec{
			Ciphers: []string{
				"ECDHE-RSA-AES128-GCM-SHA256",
				"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			},
			MinTLSVersion: confv1.VersionTLS12,
		},
	}

	_, ciphers, unsupported, err := tlsprofile.TLSConfigFromProfile(spec)

	require.NoError(t, err)
	assert.Empty(t, unsupported)
	assert.Equal(t, []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}, ciphers)
}

func TestTLSConfigFromProfile_RejectsNoUsableCipherSuites(t *testing.T) {
	spec := tlsprofile.ProfileSpec{
		Type: tlsprofile.ProfileCustom,
		TLSProfileSpec: confv1.TLSProfileSpec{
			Ciphers:       []string{"TLS_AES_128_GCM_SHA256"},
			MinTLSVersion: confv1.VersionTLS12,
		},
	}

	_, ciphers, unsupported, err := tlsprofile.TLSConfigFromProfile(spec)

	require.ErrorContains(t, err, "no configured cipher suites are usable")
	assert.Empty(t, ciphers)
	assert.Equal(t, []string{"TLS_AES_128_GCM_SHA256"}, unsupported)
}

func TestTLSConfigFromProfile_TLS13EmptyCipherList(t *testing.T) {
	spec := tlsprofile.ProfileSpec{
		Type: tlsprofile.ProfileCustom,
		TLSProfileSpec: confv1.TLSProfileSpec{
			MinTLSVersion: confv1.VersionTLS13,
		},
	}

	minVersion, ciphers, unsupported, err := tlsprofile.TLSConfigFromProfile(spec)

	require.NoError(t, err)
	assert.Equal(t, uint16(tls.VersionTLS13), minVersion)
	assert.Empty(t, ciphers)
	assert.Empty(t, unsupported)
}
