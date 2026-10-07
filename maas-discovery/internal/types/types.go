package types

// TenantsResponse represents the response for GET /v1/tenants.
type TenantsResponse struct {
	Tenants []TenantInfo `json:"tenants"`
}

// TenantInfo contains tenant identification and gateway metadata.
type TenantInfo struct {
	Name    string          `json:"name"`
	Gateway GatewayMetadata `json:"gateway"`
}

// GatewayMetadata contains gateway connection details.
type GatewayMetadata struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	Protocol    string `json:"protocol"`
	ExternalURL string `json:"externalUrl"`
	Port        int64  `json:"port"`
}
