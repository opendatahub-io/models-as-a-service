package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEffectivePayloadProcessingBackend(t *testing.T) {
	tests := []struct {
		name string
		spec AITenantSpec
		want string
	}{
		{
			name: "absent payloadProcessing means ipp",
			spec: AITenantSpec{},
			want: PayloadProcessingBackendIPP,
		},
		{
			name: "empty payloadProcessing object means ipp",
			spec: AITenantSpec{
				PayloadProcessing: &AITenantPayloadProcessing{},
			},
			want: PayloadProcessingBackendIPP,
		},
		{
			name: "praxis type resolves to praxis",
			spec: AITenantSpec{
				PayloadProcessing: &AITenantPayloadProcessing{
					Type: PayloadProcessingBackendPraxis,
				},
			},
			want: PayloadProcessingBackendPraxis,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, EffectivePayloadProcessingBackend(tt.spec))
		})
	}
}
