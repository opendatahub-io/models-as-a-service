package constant

import "time"

const (
	TierMappingConfigMap    = "tier-to-group-mapping"
	DefaultNamespace        = "maas-api"
	DefaultGatewayName      = "maas-default-gateway"
	DefaultGatewayNamespace = "openshift-ingress"

	DefaultResyncPeriod = 8 * time.Hour

	// Header configuration constants.
	HeaderUsername = "X-MaaS-Username"
	HeaderGroup    = "X-MaaS-Group"

	// API Key configuration defaults.
	// DefaultAPIKeyMaxExpirationDays is the default maximum allowed expiration for API keys.
	DefaultAPIKeyMaxExpirationDays = 30

	// LLMInferenceService annotation keys for model metadata.
	AnnotationGenAIUseCase = "opendatahub.io/genai-use-case"
	AnnotationDescription  = "openshift.io/description"
	AnnotationDisplayName  = "openshift.io/display-name"
)
