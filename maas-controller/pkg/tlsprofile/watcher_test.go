package tlsprofile

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProfileEqual(t *testing.T) {
	base := ProfileSpec{
		Type:          ProfileIntermediate,
		Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"},
		MinTLSVersion: "VersionTLS12",
	}

	tests := []struct {
		name  string
		other ProfileSpec
		want  bool
	}{
		{
			"identical",
			ProfileSpec{
				Type:          ProfileIntermediate,
				Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"},
				MinTLSVersion: "VersionTLS12",
			},
			true,
		},
		{
			"different type",
			ProfileSpec{
				Type:          ProfileModern,
				Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"},
				MinTLSVersion: "VersionTLS12",
			},
			false,
		},
		{
			"different minVersion",
			ProfileSpec{
				Type:          ProfileIntermediate,
				Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"},
				MinTLSVersion: "VersionTLS13",
			},
			false,
		},
		{
			"different ciphers",
			ProfileSpec{
				Type:          ProfileIntermediate,
				Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256"},
				MinTLSVersion: "VersionTLS12",
			},
			false,
		},
		{
			"different cipher order",
			ProfileSpec{
				Type:          ProfileIntermediate,
				Ciphers:       []string{"ECDHE-RSA-AES256-GCM-SHA384", "ECDHE-RSA-AES128-GCM-SHA256"},
				MinTLSVersion: "VersionTLS12",
			},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, profileEqual(base, tt.other))
		})
	}
}
