/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package tenantreconcile

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

var postrenderForbiddenHasOnRequestHeaders = regexp.MustCompile(`has\s*\(\s*request\.headers(?:\s*\[|\.)`)

type postrenderCELOccurrence struct {
	Path string
	Expr string
}

func collectPostrenderAuthPolicyCEL(path string, v any) []postrenderCELOccurrence {
	switch x := postrenderNormalizeYAMLValue(v).(type) {
	case nil:
		return nil
	case map[string]any:
		var out []postrenderCELOccurrence
		for k, val := range x {
			child := postrenderJoinCELPath(path, k)
			if k == "predicate" || k == "expression" {
				if s, ok := val.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, postrenderCELOccurrence{Path: child, Expr: s})
				}
				continue
			}
			if k == "selector" {
				if s, ok := val.(string); ok && strings.Contains(s, "request.") {
					out = append(out, postrenderCELOccurrence{Path: child, Expr: s})
				}
				continue
			}
			out = append(out, collectPostrenderAuthPolicyCEL(child, val)...)
		}
		return out
	case []any:
		var out []postrenderCELOccurrence
		for i, item := range x {
			out = append(out, collectPostrenderAuthPolicyCEL(fmt.Sprintf("%s[%d]", path, i), item)...)
		}
		return out
	default:
		return nil
	}
}

func postrenderNormalizeYAMLValue(v any) any {
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

func postrenderJoinCELPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

func assertPostrenderAuthPolicyCEL_RHCLCompatible(t *testing.T, spec map[string]any) {
	t.Helper()
	occurrences := collectPostrenderAuthPolicyCEL("spec", spec)
	require.NotEmpty(t, occurrences)
	for _, occ := range occurrences {
		if postrenderForbiddenHasOnRequestHeaders.MatchString(occ.Expr) {
			t.Fatalf("CEL at %s uses has(request.headers...) on RHCL 1.4.2: %q", occ.Path, occ.Expr)
		}
	}
}

func TestPatchAuthPolicyWithOIDC_AllCEL_RHCLCompatible(t *testing.T) {
	resource := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}
	oidc := &maasv1alpha1.TenantExternalOIDCConfig{
		IssuerURL:               "https://issuer.example.com/realms/test",
		ClientID:                "maas-api",
		Audience:                "maas-api",
		AccessTokenPurposeClaim: "token_use",
		AccessTokenPurpose:      "access",
	}
	require.NoError(t, patchAuthPolicyWithOIDC(logr.Discard(), resource, oidc))

	spec, ok := resource.Object["spec"].(map[string]any)
	require.True(t, ok)
	assertPostrenderAuthPolicyCEL_RHCLCompatible(t, spec)
}
