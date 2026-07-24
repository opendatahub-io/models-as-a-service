package tlsprofile_test

import (
	"testing"

	confv1 "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/tlsprofile"
)

func TestProfileEqual(t *testing.T) {
	base := tlsprofile.ProfileSpec{
		Type: tlsprofile.ProfileIntermediate,
		TLSProfileSpec: confv1.TLSProfileSpec{
			Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"},
			MinTLSVersion: confv1.VersionTLS12,
		},
	}

	tests := []struct {
		name  string
		other tlsprofile.ProfileSpec
		want  bool
	}{
		{
			"identical",
			tlsprofile.ProfileSpec{
				Type: tlsprofile.ProfileIntermediate,
				TLSProfileSpec: confv1.TLSProfileSpec{
					Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"},
					MinTLSVersion: confv1.VersionTLS12,
				},
			},
			true,
		},
		{
			"different type",
			tlsprofile.ProfileSpec{
				Type: tlsprofile.ProfileModern,
				TLSProfileSpec: confv1.TLSProfileSpec{
					Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"},
					MinTLSVersion: confv1.VersionTLS12,
				},
			},
			false,
		},
		{
			"different minVersion",
			tlsprofile.ProfileSpec{
				Type: tlsprofile.ProfileIntermediate,
				TLSProfileSpec: confv1.TLSProfileSpec{
					Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"},
					MinTLSVersion: confv1.VersionTLS13,
				},
			},
			false,
		},
		{
			"different ciphers",
			tlsprofile.ProfileSpec{
				Type: tlsprofile.ProfileIntermediate,
				TLSProfileSpec: confv1.TLSProfileSpec{
					Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256"},
					MinTLSVersion: confv1.VersionTLS12,
				},
			},
			false,
		},
		{
			"different cipher order",
			tlsprofile.ProfileSpec{
				Type: tlsprofile.ProfileIntermediate,
				TLSProfileSpec: confv1.TLSProfileSpec{
					Ciphers:       []string{"ECDHE-RSA-AES256-GCM-SHA384", "ECDHE-RSA-AES128-GCM-SHA256"},
					MinTLSVersion: confv1.VersionTLS12,
				},
			},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tlsprofile.ProfileEqual(base, tt.other))
		})
	}
}

func TestSettingsEqualIncludesAdherence(t *testing.T) {
	profile := tlsprofile.DefaultProfile()
	base := tlsprofile.Settings{
		Profile:   profile,
		Adherence: confv1.TLSAdherencePolicyNoOpinion,
	}

	assert.True(t, tlsprofile.SettingsEqual(base, base))
	assert.False(t, tlsprofile.SettingsEqual(base, tlsprofile.Settings{
		Profile:   profile,
		Adherence: confv1.TLSAdherencePolicyStrictAllComponents,
	}))
}

func TestSettingsEventHandlerTriggersForRecoveredClusterSettings(t *testing.T) {
	initial := tlsprofile.DefaultSettings()
	apiServer := newAPIServerObj(&confv1.TLSSecurityProfile{Type: confv1.TLSProfileModernType})
	apiServer.Spec.TLSAdherence = confv1.TLSAdherencePolicyStrictAllComponents

	var oldSettings, newSettings tlsprofile.Settings
	called := 0
	handler := tlsprofile.SettingsEventHandler(initial, func(old, current tlsprofile.Settings) {
		called++
		oldSettings = old
		newSettings = current
	})
	handler(apiServer)

	require.Equal(t, 1, called)
	assert.Equal(t, initial, oldSettings)
	assert.Equal(t, tlsprofile.ProfileModern, newSettings.Profile.Type)
	assert.Equal(t, confv1.TLSAdherencePolicyStrictAllComponents, newSettings.Adherence)
}

func TestSettingsEventHandlerIgnoresIrrelevantEvents(t *testing.T) {
	initial := tlsprofile.DefaultSettings()
	called := 0
	handler := tlsprofile.SettingsEventHandler(initial, func(_, _ tlsprofile.Settings) {
		called++
	})

	handler("not an APIServer")
	handler(&confv1.APIServer{ObjectMeta: metav1.ObjectMeta{Name: "not-cluster"}})
	handler(newAPIServerObj(nil))

	assert.Zero(t, called)
}
