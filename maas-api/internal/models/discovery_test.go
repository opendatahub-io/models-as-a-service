package models_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/models"
)

func TestNewManager(t *testing.T) {
	t.Run("returns error when logger is nil", func(t *testing.T) {
		manager, err := models.NewManager(nil, 15, "")
		require.Error(t, err)
		assert.Nil(t, manager)
		assert.Contains(t, err.Error(), "log is required")
	})

	t.Run("creates manager successfully with valid logger", func(t *testing.T) {
		log := logger.New(true)

		manager, err := models.NewManager(log, 15, "")
		require.NoError(t, err)
		assert.NotNil(t, manager)
	})
}

func TestBuildClusterTLSConfig(t *testing.T) {
	t.Run("returns error when logger is nil", func(t *testing.T) {
		tlsConfig, err := models.BuildClusterTLSConfig(nil)
		require.Error(t, err)
		assert.Nil(t, tlsConfig)
		assert.Contains(t, err.Error(), "log is required")
	})

	t.Run("returns secure TLS config without InsecureSkipVerify", func(t *testing.T) {
		log := logger.New(true)

		tlsConfig, err := models.BuildClusterTLSConfig(log)
		require.NoError(t, err)
		require.NotNil(t, tlsConfig)

		assert.False(t, tlsConfig.InsecureSkipVerify,
			"InsecureSkipVerify must be false for FIPS compliance")
		assert.Equal(t, uint16(tls.VersionTLS12), tlsConfig.MinVersion,
			"MinVersion must be TLS 1.2 for FIPS compliance")
		assert.NotNil(t, tlsConfig.RootCAs,
			"RootCAs must be populated with system CAs (and cluster CA when in-cluster)")
	})
}

func TestBuildClusterTLSConfigFromPath(t *testing.T) {
	t.Run("returns error when logger is nil", func(t *testing.T) {
		tlsConfig, err := models.BuildClusterTLSConfigFromPath(nil, "/nonexistent")
		require.Error(t, err)
		assert.Nil(t, tlsConfig)
	})

	t.Run("uses system root CAs when CA file is absent", func(t *testing.T) {
		log := logger.New(true)

		tlsConfig, err := models.BuildClusterTLSConfigFromPath(log, "/nonexistent/ca.crt")
		require.NoError(t, err)
		require.NotNil(t, tlsConfig)

		assert.False(t, tlsConfig.InsecureSkipVerify)
		assert.Equal(t, uint16(tls.VersionTLS12), tlsConfig.MinVersion)
		assert.NotNil(t, tlsConfig.RootCAs)
	})

	t.Run("returns error when CA file exists but contains invalid PEM", func(t *testing.T) {
		log := logger.New(true)

		f, err := os.CreateTemp(t.TempDir(), "ca-*.crt")
		require.NoError(t, err)
		_, err = f.WriteString("NOT VALID PEM CONTENT")
		require.NoError(t, err)
		require.NoError(t, f.Close())

		tlsConfig, err := models.BuildClusterTLSConfigFromPath(log, f.Name())
		require.Error(t, err)
		assert.Nil(t, tlsConfig)
		assert.Contains(t, err.Error(), "failed to parse")
	})

	t.Run("returns error when CA file exists but is unreadable", func(t *testing.T) {
		log := logger.New(true)

		dir := t.TempDir()
		caPath := filepath.Join(dir, "ca.crt")
		require.NoError(t, os.WriteFile(caPath, []byte("placeholder"), 0o000))
		t.Cleanup(func() { _ = os.Chmod(caPath, 0o644) })

		tlsConfig, err := models.BuildClusterTLSConfigFromPath(log, caPath)
		require.Error(t, err)
		assert.Nil(t, tlsConfig)
	})

	t.Run("appends valid CA cert to system pool", func(t *testing.T) {
		log := logger.New(true)

		certPEM := selfSignedCertPEM(t)
		f, err := os.CreateTemp(t.TempDir(), "ca-*.crt")
		require.NoError(t, err)
		_, err = f.Write(certPEM)
		require.NoError(t, err)
		require.NoError(t, f.Close())

		tlsConfig, err := models.BuildClusterTLSConfigFromPath(log, f.Name())
		require.NoError(t, err)
		require.NotNil(t, tlsConfig)

		assert.False(t, tlsConfig.InsecureSkipVerify)
		assert.Equal(t, uint16(tls.VersionTLS12), tlsConfig.MinVersion)
		assert.NotNil(t, tlsConfig.RootCAs)
	})
}

// selfSignedCertPEM generates a minimal self-signed CA certificate in PEM format for use in tests.
func selfSignedCertPEM(t *testing.T) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
}
