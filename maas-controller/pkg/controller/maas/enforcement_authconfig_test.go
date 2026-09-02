/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package maas

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func loadOracleAuthConfig(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "oracle-authconfig-site-a.json"))
	if err != nil {
		t.Fatalf("read oracle authconfig: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse oracle authconfig: %v", err)
	}
	return spec
}

func TestBuildAuthConfigSpecWrapsRules(t *testing.T) {
	rules := map[string]any{
		"authentication": map[string]any{"api-keys": "x"},
		"metadata":       map[string]any{"apiKeyValidation": "y"},
		"authorization":  map[string]any{"auth-valid": "z"},
		"response":       map[string]any{"success": "w"},
		"ignored":        "should not appear",
	}
	spec := buildAuthConfigSpec("the-host", rules)

	if hosts, ok := spec["hosts"].([]any); !ok || len(hosts) != 1 || hosts[0] != "the-host" {
		t.Errorf("hosts = %v, want [the-host]", spec["hosts"])
	}
	for _, section := range authConfigSections {
		if !reflect.DeepEqual(spec[section], rules[section]) {
			t.Errorf("section %s not carried through", section)
		}
	}
	if _, ok := spec["ignored"]; ok {
		t.Errorf("non-rule key leaked into the AuthConfig spec")
	}
}

func TestGatewayAuthPolicyRulesMatchOracle(t *testing.T) {
	r := &MaaSAuthPolicyReconciler{
		InfraNamespace:   "redhat-ai-gateway-infra",
		ClusterAudience:  "https://kubernetes.default.svc",
		MetadataCacheTTL: 60,
	}
	// A non-empty tenantID yields a tenant-scoped maas-api service name in the URLs.
	spec := r.buildGatewayAuthPolicySpec(nil, false, "site-a", "site-a", "openshift-ingress", "maas-site-a-gateway")

	rules, ok := authPolicyRules(spec)
	if !ok {
		t.Fatalf("could not extract defaults.rules from AuthPolicy spec")
	}
	ac := buildAuthConfigSpec("ignored-host", rules)
	oracle := loadOracleAuthConfig(t)

	// Authentication is data-independent and must match the oracle exactly.
	if got, want := jsonNormalize(t, ac["authentication"]), jsonNormalize(t, oracle["authentication"]); !reflect.DeepEqual(got, want) {
		gj := mustJSON(t, got)
		wj := mustJSON(t, want)
		t.Errorf("AuthConfig authentication does not match oracle:\n--- got ---\n%s\n--- want ---\n%s", gj, wj)
	}

	// Metadata: the oracle may encode equivalent CEL differently, so compare the
	// functional fields, not the raw CEL.
	gotMeta, _ := ac["metadata"].(map[string]any)
	wantMeta, ok := oracle["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("oracle metadata is not a map")
	}
	if len(gotMeta) != len(wantMeta) {
		t.Fatalf("metadata callouts: got %d, want %d", len(gotMeta), len(wantMeta))
	}
	for name, wv := range wantMeta {
		gv, ok := gotMeta[name].(map[string]any)
		if !ok {
			t.Errorf("metadata callout %q missing", name)
			continue
		}
		wm, ok := wv.(map[string]any)
		if !ok {
			t.Errorf("oracle metadata callout %q is not a map", name)
			continue
		}
		gHTTP, _ := gv["http"].(map[string]any)
		wHTTP, _ := wm["http"].(map[string]any)
		for _, f := range []string{"url", "method", "contentType"} {
			if gHTTP[f] != wHTTP[f] {
				t.Errorf("metadata %q http.%s = %v, want %v", name, f, gHTTP[f], wHTTP[f])
			}
		}
		gCache, _ := gv["cache"].(map[string]any)
		wCache, _ := wm["cache"].(map[string]any)
		if !reflect.DeepEqual(jsonNormalize(t, gCache["ttl"]), jsonNormalize(t, wCache["ttl"])) {
			t.Errorf("metadata %q cache.ttl = %v, want %v", name, gCache["ttl"], wCache["ttl"])
		}
	}
}
