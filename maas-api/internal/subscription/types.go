package subscription

// SelectRequest contains the user information for subscription selection.
type SelectRequest struct {
	Groups                []string `json:"groups"`                                // User's group memberships (optional if username provided)
	Username              string   `binding:"required"           json:"username"` // User's username
	RequestedSubscription string   `json:"requestedSubscription"`                 // Optional explicit subscription name
}

// SelectResponse contains the selected subscription details.
type SelectResponse struct {
	Name           string            `json:"name"`                     // Subscription name
	OrganizationID string            `json:"organizationId,omitempty"` // Organization ID for billing
	CostCenter     string            `json:"costCenter,omitempty"`     // Cost center for attribution
	Labels         map[string]string `json:"labels,omitempty"`         // Additional tracking labels
}

// ErrorResponse represents an error response.
type ErrorResponse struct {
	Error   string `json:"error"`   // Error code (e.g., "bad_request", "not_found")
	Message string `json:"message"` // Human-readable error message
}
