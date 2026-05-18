package tlsprofile_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/tlsprofile"
)

func TestDefaultProfile(t *testing.T) {
	p := tlsprofile.DefaultProfile()

	assert.Equal(t, tlsprofile.ProfileIntermediate, p.Type)
	assert.Equal(t, "VersionTLS12", p.MinTLSVersion)
	assert.NotEmpty(t, p.Ciphers)
}

func TestDefaultProfileReturnsDeepCopy(t *testing.T) {
	p1 := tlsprofile.DefaultProfile()
	p2 := tlsprofile.DefaultProfile()

	p1.Ciphers[0] = "MUTATED"
	assert.NotEqual(t, p1.Ciphers[0], p2.Ciphers[0], "mutating one DefaultProfile must not affect another")
}

func TestProfilesContainExpectedCiphers(t *testing.T) {
	intermediate := tlsprofile.Profiles[tlsprofile.ProfileIntermediate]
	assert.Contains(t, intermediate.Ciphers, "ECDHE-ECDSA-AES128-GCM-SHA256")
	assert.Contains(t, intermediate.Ciphers, "TLS_AES_128_GCM_SHA256")

	old := tlsprofile.Profiles[tlsprofile.ProfileOld]
	assert.Contains(t, old.Ciphers, "DES-CBC3-SHA")
	assert.Greater(t, len(old.Ciphers), len(intermediate.Ciphers))

	modern := tlsprofile.Profiles[tlsprofile.ProfileModern]
	assert.Less(t, len(modern.Ciphers), len(intermediate.Ciphers))
}
