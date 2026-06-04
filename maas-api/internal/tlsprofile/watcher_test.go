package tlsprofile_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/tlsprofile"
)

func TestProfileEqual(t *testing.T) {
	base := tlsprofile.ProfileSpec{
		Type:          tlsprofile.ProfileIntermediate,
		Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"},
		MinTLSVersion: "VersionTLS12",
	}

	tests := []struct {
		name  string
		other tlsprofile.ProfileSpec
		want  bool
	}{
		{
			"identical",
			tlsprofile.ProfileSpec{
				Type:          tlsprofile.ProfileIntermediate,
				Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"},
				MinTLSVersion: "VersionTLS12",
			},
			true,
		},
		{
			"different type",
			tlsprofile.ProfileSpec{
				Type:          tlsprofile.ProfileModern,
				Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"},
				MinTLSVersion: "VersionTLS12",
			},
			false,
		},
		{
			"different minVersion",
			tlsprofile.ProfileSpec{
				Type:          tlsprofile.ProfileIntermediate,
				Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"},
				MinTLSVersion: "VersionTLS13",
			},
			false,
		},
		{
			"different ciphers",
			tlsprofile.ProfileSpec{
				Type:          tlsprofile.ProfileIntermediate,
				Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256"},
				MinTLSVersion: "VersionTLS12",
			},
			false,
		},
		{
			"different cipher order",
			tlsprofile.ProfileSpec{
				Type:          tlsprofile.ProfileIntermediate,
				Ciphers:       []string{"ECDHE-RSA-AES256-GCM-SHA384", "ECDHE-RSA-AES128-GCM-SHA256"},
				MinTLSVersion: "VersionTLS12",
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
