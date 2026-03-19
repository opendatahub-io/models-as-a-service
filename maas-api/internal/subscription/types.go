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
	ModelRefs      []ModelRef        `json:"modelRefs,omitempty"`      // Models included in this subscription
	OrganizationID string            `json:"organizationId,omitempty"` // Organization ID for billing
	CostCenter     string            `json:"costCenter,omitempty"`     // Cost center for attribution
	Labels         map[string]string `json:"labels,omitempty"`         // Additional tracking labels

	// Error fields (populated when selection fails)
	Error   string `json:"error,omitempty"`   // Error code (e.g., "bad_request", "not_found", "access_denied", "multiple_subscriptions")
	Message string `json:"message,omitempty"` // Human-readable error message
}

// ErrorResponse represents an error response (deprecated - use SelectResponse instead).
type ErrorResponse struct {
	Error   string `json:"error"`   // Error code (e.g., "bad_request", "not_found")
	Message string `json:"message"` // Human-readable error message
}
