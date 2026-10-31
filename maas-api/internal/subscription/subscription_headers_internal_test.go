package subscription

import "testing"

func TestParseSubscriptionHeaders_RejectsReservedAndInvalidNames(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "reserved authorization", value: `{"Authorization":["value"]}`},
		{name: "non-grid header", value: `{"x-maas-username":["value"]}`},
		{name: "invalid header name", value: `{"bad header":["value"]}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseSubscriptionHeaders(test.value); err == nil {
				t.Fatalf("parseSubscriptionHeaders(%q) succeeded, want error", test.value)
			}
		})
	}
}

func TestParseSubscriptionHeaders_NormalizesCaseInsensitiveDuplicates(t *testing.T) {
	if _, err := parseSubscriptionHeaders(`{"X-Custom":["one"],"x-custom":["two"]}`); err == nil {
		t.Fatal("parseSubscriptionHeaders() succeeded, want duplicate header error")
	}
}

func TestParseSubscriptionHeaders_AllowsHTTPValidHeaderValueWhitespace(t *testing.T) {
	headers, err := parseSubscriptionHeaders(`{"x-grid-custom-header":["region1","\tregion2"]}`)
	if err != nil {
		t.Fatalf("parseSubscriptionHeaders() error = %v", err)
	}
	if values := headers["x-grid-custom-header"]; len(values) != 2 || values[1] != "\tregion2" {
		t.Fatalf("x-grid-custom-header values = %#v, want horizontal tab preserved", values)
	}
}

func TestParseSubscriptionHeaders_RejectsNullValues(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "top-level null", value: "null"},
		{name: "null values", value: `{"x-grid-custom-header":null}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseSubscriptionHeaders(test.value); err == nil {
				t.Fatalf("parseSubscriptionHeaders(%q) succeeded, want null error", test.value)
			}
		})
	}
}
