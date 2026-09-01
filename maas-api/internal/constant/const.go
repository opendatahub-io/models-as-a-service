package constant

import "time"

const (
	DefaultNamespace                 = "maas-api"
	DefaultGatewayName               = "maas-default-gateway"
	DefaultGatewayNamespace          = "openshift-ingress"
	DefaultMaaSSubscriptionNamespace = "models-as-a-service"

	DefaultResyncPeriod = 8 * time.Hour

	DefaultMetricsPort = 9090

	// Header configuration constants.
	HeaderUsername = "X-MaaS-Username"
	HeaderGroup    = "X-MaaS-Group"

	// API Key configuration defaults.
	// DefaultAPIKeyMaxExpirationDays is the default maximum allowed expiration for API keys.
	DefaultAPIKeyMaxExpirationDays = 90

	// DefaultEphemeralKeyMaxExpiration is the maximum allowed expiration for ephemeral API keys.
	DefaultEphemeralKeyMaxExpiration = 1 * time.Hour

	// DefaultSARCacheMaxSize is the maximum number of entries in the SAR admin-check cache.
	DefaultSARCacheMaxSize = 8192

	// LLMInferenceService annotation keys for model metadata.
	AnnotationGenAIUseCase      = "opendatahub.io/genai-use-case"
	AnnotationDescription       = "openshift.io/description"
	AnnotationDisplayName       = "openshift.io/display-name"
	AnnotationContextWindow     = "opendatahub.io/context-window"
	AnnotationModelCapabilities = "opendatahub.io/model-capabilities"

	// MaxLabelsEntries is the maximum number of label key-value pairs per API key (prevent abuse).
	MaxLabelsEntries    = 50
	MaxLabelKeyLength   = 128
	MaxLabelValueLength = 1024

	// Rejection reason constants for metrics.
	RejectionRateLimited   = "rate-limited"
	RejectionUnauthorized  = "unauthorized"
	RejectionNoCapacity    = "no-capacity"
	RejectionQuotaExceeded = "quota-exceeded"
)
