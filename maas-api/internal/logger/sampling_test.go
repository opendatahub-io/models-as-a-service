package logger_test

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

func TestProductionOTelJSONPreservesZapSampling(t *testing.T) {
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	previousStdout := os.Stdout
	os.Stdout = writer
	log := logger.NewWithFormat(false, logger.FormatOTelJSON)
	os.Stdout = previousStdout

	for range 101 {
		log.Info("repeated")
	}
	_ = log.Sync() // Sync on a pipe is not supported on every platform.
	require.NoError(t, writer.Close())
	output, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())

	assert.Len(t, strings.Split(strings.TrimSpace(string(output)), "\n"), 100)
	var record map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.Split(strings.TrimSpace(string(output)), "\n")[0]), &record))
	assert.Equal(t, "maas-api", record["logger"])
}
