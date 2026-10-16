/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package maas

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// RHCL 1.4.2 / Authorino v1.4.2 rejects has() on request.headers map keys (subscript or field access) (RHOAIENG-84699).
var forbiddenHasOnRequestHeaders = regexp.MustCompile(`has\s*\(\s*request\.headers(?:\s*\[|\.)`)

type authPolicyCELOccurrence struct {
	Path string
	Expr string
}

func collectAuthPolicyCEL(path string, v any) []authPolicyCELOccurrence {
	switch x := normalizeYAMLValue(v).(type) {
	case nil:
		return nil
	case map[string]any:
		var out []authPolicyCELOccurrence
		for k, val := range x {
			child := joinCELPath(path, k)
			if k == "predicate" || k == "expression" {
				if s, ok := val.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, authPolicyCELOccurrence{Path: child, Expr: s})
				}
				continue
			}
			if k == "selector" {
				if s, ok := val.(string); ok && strings.Contains(s, "request.") {
					out = append(out, authPolicyCELOccurrence{Path: child, Expr: s})
				}
				continue
			}
			out = append(out, collectAuthPolicyCEL(child, val)...)
		}
		return out
	case []any:
		var out []authPolicyCELOccurrence
		for i, item := range x {
			out = append(out, collectAuthPolicyCEL(fmt.Sprintf("%s[%d]", path, i), item)...)
		}
		return out
	default:
		return nil
	}
}

func normalizeYAMLValue(v any) any {
	switch x := v.(type) {
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			m[fmt.Sprint(k)] = val
		}
		return m
	case map[string]any:
		return x
	case []any:
		return x
	default:
		return v
	}
}

func joinCELPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

func assertRHCLCompatibleCEL(t *testing.T, path, expr string) {
	t.Helper()
	if forbiddenHasOnRequestHeaders.MatchString(expr) {
		t.Fatalf("CEL at %s uses has(request.headers...) which Authorino v1.4.2 rejects; use (\"header\" in request.headers) instead: %q", path, expr)
	}
}

func TestRejectRHCLHasOnRequestHeadersCEL(t *testing.T) {
	for _, expr := range []string{
		`!has(request.headers["x-maas-username"])`,
		`has (request.headers.authorization)`,
		`has( request.headers.foo)`,
	} {
		if !forbiddenHasOnRequestHeaders.MatchString(expr) {
			t.Errorf("expected rejection for %q", expr)
		}
	}
}

func assertAllAuthPolicyCEL_RHCLCompatible(t *testing.T, spec map[string]any) {
	t.Helper()
	occurrences := collectAuthPolicyCEL("spec", spec)
	if len(occurrences) == 0 {
		t.Fatal("expected at least one CEL expression in AuthPolicy spec")
	}
	for _, occ := range occurrences {
		assertRHCLCompatibleCEL(t, occ.Path, occ.Expr)
	}
}

func TestGatewayAuthPolicySpec_AllCEL_RHCLCompatible(t *testing.T) {
	sampleOIDC := &oidcConfig{
		IssuerURL:               "https://issuer.example.com/realms/test",
		ClientID:                "maas-client",
		Audience:                "maas-api",
		AccessTokenPurposeClaim: "token_use",
		AccessTokenPurpose:      "access",
		TTL:                     300,
	}

	r := &MaaSAuthPolicyReconciler{
		InfraNamespace:   "maas-system",
		GatewayName:      "maas-default-gateway",
		GatewayNamespace: "gateway-ns",
		ClusterAudience:  "https://kubernetes.default.svc",
		MetadataCacheTTL: 60,
		AuthzCacheTTL:    60,
	}

	cases := []struct {
		name           string
		oidc           *oidcConfig
		xAPIKeyEnabled bool
		tenantID       string
		tenantName     string
		gatewayNS      string
		gatewayName    string
	}{
		{
			name:           "default_gateway",
			oidc:           nil,
			xAPIKeyEnabled: false,
			tenantName:     "models-as-a-service",
			gatewayNS:      "test-gateway-ns",
			gatewayName:    "test-gateway",
		},
		{
			name:           "oidc_enabled",
			oidc:           sampleOIDC,
			xAPIKeyEnabled: false,
			tenantName:     "models-as-a-service",
			gatewayNS:      "test-gateway-ns",
			gatewayName:    "test-gateway",
		},
		{
			name:           "x_api_key_enabled",
			oidc:           nil,
			xAPIKeyEnabled: true,
			tenantName:     "models-as-a-service",
			gatewayNS:      "gateway-ns",
			gatewayName:    "maas-default-gateway",
		},
		{
			name:           "tenant_gateway",
			oidc:           nil,
			xAPIKeyEnabled: false,
			tenantID:       "tenant-abc",
			tenantName:     "tenant-ns",
			gatewayNS:      "tenant-gw-ns",
			gatewayName:    "tenant-gateway",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := r.buildGatewayAuthPolicySpec(
				tc.oidc, tc.xAPIKeyEnabled, tc.tenantID, tc.tenantName, tc.gatewayNS, tc.gatewayName,
			)
			assertAllAuthPolicyCEL_RHCLCompatible(t, spec)
		})
	}
}

func TestMaaSAPIAuthPolicyYAML_CEL_RHCLCompatible(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "deployment", "base", "maas-api", "policies", "auth-policy.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth-policy.yaml: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode auth-policy.yaml: %v", err)
	}
	spec, ok := doc["spec"].(map[string]any)
	if !ok {
		// yaml.v3 may decode nested maps as map[interface{}]interface{}
		if m, ok := doc["spec"].(map[any]any); ok {
			spec = make(map[string]any, len(m))
			for k, v := range m {
				spec[fmt.Sprint(k)] = v
			}
		}
	}
	if spec == nil {
		t.Fatal("auth-policy.yaml missing spec map")
	}
	assertAllAuthPolicyCEL_RHCLCompatible(t, spec)
}
