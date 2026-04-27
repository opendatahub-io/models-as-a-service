package models_test

import (
	"crypto/tls"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/models"
)

func TestNewManager(t *testing.T) {
	t.Run("returns error when logger is nil", func(t *testing.T) {
		manager, err := models.NewManager(nil, 15)
		require.Error(t, err)
		assert.Nil(t, manager)
		assert.Contains(t, err.Error(), "log is required")
	})

	t.Run("creates manager successfully with valid logger", func(t *testing.T) {
		log := logger.New(true)

		manager, err := models.NewManager(log, 15)
		require.NoError(t, err)
		assert.NotNil(t, manager)
	})
}

func TestBuildClusterTLSConfig(t *testing.T) {
	t.Run("returns secure TLS config", func(t *testing.T) {
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
