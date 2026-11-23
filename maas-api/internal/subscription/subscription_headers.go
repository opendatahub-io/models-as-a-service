package subscription

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	httpguts "golang.org/x/net/http/httpguts"
)

// The experimental subscription-header annotation is a contract shared with
// maas-controller/pkg/controller/maas/subscription_headers.go. Keep parsing and
// validation semantics in sync so selection and rendered AuthPolicies agree.
func parseSubscriptionHeaders(value string) (map[string][]string, error) {
	if value == "" {
		return nil, nil
	}

	var headers map[string][]string
	if err := json.Unmarshal([]byte(value), &headers); err != nil {
		return nil, err
	}
	if headers == nil {
		return nil, errors.New("subscription headers must be a JSON object")
	}

	for name := range headers {
		if !validSubscriptionHeaderName(name) {
			return nil, fmt.Errorf("invalid header name %q", name)
		}
		values := headers[name]
		if values == nil {
			return nil, fmt.Errorf("null values for header %q", name)
		}
		for _, headerValue := range values {
			if !validSubscriptionHeaderValue(headerValue) {
				return nil, fmt.Errorf("invalid value for header %q", name)
			}
		}
	}

	normalized := make(map[string][]string, len(headers))
	for name, values := range headers {
		lowerName := strings.ToLower(name)
		if _, exists := normalized[lowerName]; exists {
			return nil, fmt.Errorf("duplicate case-insensitive header name %q", lowerName)
		}
		normalized[lowerName] = values
	}
	return normalized, nil
}

func validSubscriptionHeaderName(name string) bool {
	if !httpguts.ValidHeaderFieldName(name) {
		return false
	}

	lowerName := strings.ToLower(name)
	return strings.HasPrefix(lowerName, "x-grid-")
}

func validSubscriptionHeaderValue(value string) bool {
	return httpguts.ValidHeaderFieldValue(value)
}
