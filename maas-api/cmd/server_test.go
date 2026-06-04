package main

import (
	"crypto/tls"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/config"
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
}
