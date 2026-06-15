package tlsprofile

import (
	"crypto/tls"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTLSConfigFromProfile_Intermediate(t *testing.T) {
	spec := ProfileSpec{
		Type: ProfileIntermediate,
		Ciphers: []string{
			"TLS_AES_128_GCM_SHA256",
			"TLS_AES_256_GCM_SHA384",
			"ECDHE-ECDSA-AES128-GCM-SHA256",
			"ECDHE-RSA-AES128-GCM-SHA256",
		},
		MinTLSVersion: "VersionTLS12",
	}

	minVer, ciphers, unsupported := TLSConfigFromProfile(spec)

	assert.Equal(t, uint16(tls.VersionTLS12), minVer)
	assert.Contains(t, ciphers, tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256)
	assert.Contains(t, ciphers, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256)
	assert.Empty(t, unsupported)
}

func TestTLSConfigFromProfile_TLS13CiphersSkipped(t *testing.T) {
	spec := ProfileSpec{
		Type: ProfileModern,
		Ciphers: []string{
			"TLS_AES_128_GCM_SHA256",
			"TLS_AES_256_GCM_SHA384",
			"TLS_CHACHA20_POLY1305_SHA256",
		},
		MinTLSVersion: "VersionTLS13",
	}

	minVer, ciphers, unsupported := TLSConfigFromProfile(spec)

	assert.Equal(t, uint16(tls.VersionTLS13), minVer)
	assert.Empty(t, ciphers, "TLS 1.3 ciphers should be skipped because Go manages them")
	assert.Empty(t, unsupported)
}

func TestTLSConfigFromProfile_UnsupportedCiphers(t *testing.T) {
	spec := ProfileSpec{
		Type:          ProfileCustom,
		Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256", "UNKNOWN-CIPHER-XYZ"},
		MinTLSVersion: "VersionTLS12",
	}

	_, ciphers, unsupported := TLSConfigFromProfile(spec)

	assert.Len(t, ciphers, 1)
	assert.Equal(t, []string{"UNKNOWN-CIPHER-XYZ"}, unsupported)
}

func TestNewTLSConfigFromProfile(t *testing.T) {
	spec := ProfileSpec{
		Type:          ProfileIntermediate,
		Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256"},
		MinTLSVersion: "VersionTLS12",
	}

	applyTLSConfig, unsupported := NewTLSConfigFromProfile(spec)
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2"},
	}
	applyTLSConfig(cfg)

	assert.Empty(t, unsupported)
	assert.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion)
	assert.Equal(t, []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}, cfg.CipherSuites)
	assert.Equal(t, []string{"h2"}, cfg.NextProtos, "profile callback should not alter protocol negotiation")
}
