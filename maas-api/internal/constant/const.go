package constant

import "time"

const (
	DefaultNamespace                 = "maas-api"
	DefaultGatewayName               = "maas-default-gateway"
	DefaultGatewayNamespace          = "openshift-ingress"
	DefaultMaaSSubscriptionNamespace = "models-as-a-service"

	DefaultResyncPeriod = 8 * time.Hour

	// Header configuration constants.
	HeaderUsername = "X-MaaS-Username"
	HeaderGroup    = "X-MaaS-Group"

	// API Key configuration defaults.
	// DefaultAPIKeyMaxExpirationDays is the default maximum allowed expiration for API keys.
	DefaultAPIKeyMaxExpirationDays = 90

	// DefaultSARCacheMaxSize is the maximum number of entries in the SAR admin-check cache.
	DefaultSARCacheMaxSize = 8192

	// LLMInferenceService annotation keys for model metadata.
	AnnotationGenAIUseCase  = "opendatahub.io/genai-use-case"
	AnnotationDescription   = "openshift.io/description"
	AnnotationDisplayName   = "openshift.io/display-name"
	AnnotationContextWindow = "opendatahub.io/context-window"
)
