package subscription

// SelectRequest contains the user information for subscription selection.
type SelectRequest struct {
	Groups                []string `json:"groups"`                                // User's group memberships (optional if username provided)
	Username              string   `binding:"required"           json:"username"` // User's username
	RequestedSubscription string   `json:"requestedSubscription"`                 // Optional explicit subscription name
	RequestedModel        string   `json:"requestedModel"`                        // Optional model reference (format: namespace/name) to validate subscription includes this model
}

// ModelRef represents a model reference in a subscription.
type ModelRef struct {
	Namespace string `json:"namespace"` // Model namespace
	Name      string `json:"name"`      // Model name
}

// SelectResponse contains the selected subscription details or error information.
// This always returns HTTP 200 with either success or error fields populated.
type SelectResponse struct {
	// Success fields (populated when selection succeeds)
	Name           string            `json:"name,omitempty"`           // Subscription name
	Namespace      string            `json:"namespace,omitempty"`      // Subscription namespace
	DisplayName    string            `json:"displayName,omitempty"`    // Human-friendly display name for UI
	Description    string            `json:"description,omitempty"`    // Subscription description
	Priority       int32             `json:"priority,omitempty"`       // Subscription priority
	ModelRefs      []ModelRefInfo    `json:"modelRefs,omitempty"`      // Model references with rate limits
	OrganizationID string            `json:"organizationId,omitempty"` // Organization ID for billing
	CostCenter     string            `json:"costCenter,omitempty"`     // Cost center for attribution
	Labels         map[string]string `json:"labels,omitempty"`         // Additional tracking labels

	// Error fields (populated when selection fails)
	Error   string `json:"error,omitempty"`   // Error code (e.g., "bad_request", "not_found", "access_denied", "multiple_subscriptions")
	Message string `json:"message,omitempty"` // Human-readable error message
}

// SubscriptionInfo represents a subscription in list responses.
// Contains everything from the MaaSSubscription spec except owner.
type SubscriptionInfo struct {
	SubscriptionIDHeader    string            `json:"subscription_id_header"`
	SubscriptionDescription string            `json:"subscription_description"`
	DisplayName             string            `json:"display_name,omitempty"`
	Priority                int32             `json:"priority"`
	ModelRefs               []ModelRefInfo    `json:"model_refs"`
	OrganizationID          string            `json:"organization_id,omitempty"`
	CostCenter              string            `json:"cost_center,omitempty"`
	Labels                  map[string]string `json:"labels,omitempty"`
}

// ModelRefInfo represents a model reference with its rate limits.
type ModelRefInfo struct {
	Name            string           `json:"name"`
	Namespace       string           `json:"namespace,omitempty"`
	TokenRateLimits []TokenRateLimit `json:"token_rate_limits,omitempty"`
	BillingRate     *BillingRate     `json:"billing_rate,omitempty"`
}

// TokenRateLimit defines a token rate limit.
type TokenRateLimit struct {
	Limit  int64  `json:"limit"`
	Window string `json:"window"`
}

// BillingRate defines billing information.
type BillingRate struct {
	PerToken string `json:"per_token"`
}

// ErrorResponse represents an error response (deprecated - use SelectResponse instead).
type ErrorResponse struct {
	Error   string `json:"error"`   // Error code (e.g., "bad_request", "not_found")
	Message string `json:"message"` // Human-readable error message
}
