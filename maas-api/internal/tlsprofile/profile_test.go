package tlsprofile_test

import (
	"testing"

	confv1 "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/tlsprofile"
)

func TestDefaultProfile(t *testing.T) {
	p := tlsprofile.DefaultProfile()

	assert.Equal(t, tlsprofile.ProfileIntermediate, p.Type)
	assert.Equal(t, confv1.VersionTLS12, p.MinTLSVersion)
	assert.NotEmpty(t, p.Ciphers)
}

func TestDefaultProfileReturnsDeepCopy(t *testing.T) {
	p1 := tlsprofile.DefaultProfile()
	p2 := tlsprofile.DefaultProfile()

	p1.Ciphers[0] = "MUTATED"
	assert.NotEqual(t, p1.Ciphers[0], p2.Ciphers[0], "mutating one DefaultProfile must not affect another")
}

func TestSettingsAppliedProfile(t *testing.T) {
	modern, ok := tlsprofile.LookupNamedProfile(tlsprofile.ProfileModern)
	assert.True(t, ok)

	tests := []struct {
		name      string
		adherence confv1.TLSAdherencePolicy
		wantType  tlsprofile.ProfileType
	}{
		{
			name:      "unset uses Intermediate",
			adherence: confv1.TLSAdherencePolicyNoOpinion,
			wantType:  tlsprofile.ProfileIntermediate,
		},
		{
			name:      "legacy uses Intermediate",
			adherence: confv1.TLSAdherencePolicyLegacyAdheringComponentsOnly,
			wantType:  tlsprofile.ProfileIntermediate,
		},
		{
			name:      "strict uses cluster profile",
			adherence: confv1.TLSAdherencePolicyStrictAllComponents,
			wantType:  tlsprofile.ProfileModern,
		},
		{
			name:      "unknown future value fails secure",
			adherence: confv1.TLSAdherencePolicy("FuturePolicy"),
			wantType:  tlsprofile.ProfileModern,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := tlsprofile.Settings{Profile: modern, Adherence: tt.adherence}
			assert.Equal(t, tt.wantType, settings.AppliedProfile().Type)
		})
	}
}
