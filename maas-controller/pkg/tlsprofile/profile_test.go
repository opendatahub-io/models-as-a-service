package tlsprofile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultProfile(t *testing.T) {
	p := DefaultProfile()

	assert.Equal(t, ProfileIntermediate, p.Type)
	assert.Equal(t, "VersionTLS12", p.MinTLSVersion)
	assert.NotEmpty(t, p.Ciphers)
}

func TestDefaultProfileReturnsDeepCopy(t *testing.T) {
	p1 := DefaultProfile()
	p2 := DefaultProfile()

	p1.Ciphers[0] = "MUTATED"
	assert.NotEqual(t, p1.Ciphers[0], p2.Ciphers[0], "mutating one DefaultProfile must not affect another")
}

func TestLookupNamedProfile(t *testing.T) {
	tests := []struct {
		name     string
		profType ProfileType
		wantOK   bool
		wantMinV string
		wantType ProfileType
	}{
		{"Old", ProfileOld, true, "VersionTLS10", ProfileOld},
		{"Intermediate", ProfileIntermediate, true, "VersionTLS12", ProfileIntermediate},
		{"Modern", ProfileModern, true, "VersionTLS13", ProfileModern},
		{"Unknown", ProfileType("Unknown"), false, "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, ok := LookupNamedProfile(tt.profType)
			assert.Equal(t, tt.wantOK, ok)
			if ok {
				assert.Equal(t, tt.wantMinV, spec.MinTLSVersion)
				assert.Equal(t, tt.wantType, spec.Type)
				assert.NotEmpty(t, spec.Ciphers)
			}
		})
	}
}

func TestLookupNamedProfileReturnsDeepCopy(t *testing.T) {
	p1, ok := LookupNamedProfile(ProfileIntermediate)
	require.True(t, ok)

	p2, ok := LookupNamedProfile(ProfileIntermediate)
	require.True(t, ok)

	p1.Ciphers[0] = "MUTATED"
	assert.NotEqual(t, p1.Ciphers[0], p2.Ciphers[0], "mutating one copy must not affect another")
}

func TestProfilesContainExpectedCiphers(t *testing.T) {
	intermediate, ok := LookupNamedProfile(ProfileIntermediate)
	require.True(t, ok)
	assert.Contains(t, intermediate.Ciphers, "ECDHE-ECDSA-AES128-GCM-SHA256")
	assert.Contains(t, intermediate.Ciphers, "TLS_AES_128_GCM_SHA256")

	old, ok := LookupNamedProfile(ProfileOld)
	require.True(t, ok)
	assert.Contains(t, old.Ciphers, "DES-CBC3-SHA")
	assert.Greater(t, len(old.Ciphers), len(intermediate.Ciphers))

	modern, ok := LookupNamedProfile(ProfileModern)
	require.True(t, ok)
	assert.Less(t, len(modern.Ciphers), len(intermediate.Ciphers))
}
