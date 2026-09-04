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
	neturl "net/url"
)

// AuthConfig generation. The rule content is already produced by
// buildGatewayAuthPolicySpec; Kuadrant copies those sections into an AuthConfig
// and adds a hosts selector, so we reuse that builder instead of rewriting the
// CEL-heavy auth logic.

// authConfigSections are the AuthPolicy rule sections carried into the AuthConfig.
var authConfigSections = []string{"authentication", "metadata", "authorization", "response"}

// buildAuthConfigSpec wraps AuthPolicy rules as an AuthConfig spec. host MUST
// equal the auth scope in the pluginConfig action sets so Authorino selects it.
func buildAuthConfigSpec(host string, rules map[string]any) map[string]any {
	spec := map[string]any{"hosts": []any{host}}
	for _, section := range authConfigSections {
		if v, ok := rules[section]; ok {
			spec[section] = v
		}
	}
	injectDefaultCredentials(spec)
	renameResponseFilters(spec)
	return spec
}

// renameResponseFilters translates response.success.filters to dynamicMetadata,
// matching how Kuadrant compiles an AuthPolicy into an AuthConfig.
func renameResponseFilters(spec map[string]any) {
	resp, ok := spec["response"].(map[string]any)
	if !ok {
		return
	}
	success, ok := resp["success"].(map[string]any)
	if !ok {
		return
	}
	if f, ok := success["filters"]; ok {
		success["dynamicMetadata"] = f
		delete(success, "filters")
	}
}

// injectDefaultCredentials adds the empty credentials{} Authorino expects on each
// authentication method and each metadata HTTP callout.
func injectDefaultCredentials(spec map[string]any) {
	if auth, ok := spec["authentication"].(map[string]any); ok {
		for _, m := range auth {
			ensureCredentials(m)
		}
	}
	if meta, ok := spec["metadata"].(map[string]any); ok {
		for _, m := range meta {
			if mm, ok := m.(map[string]any); ok {
				ensureCredentials(mm["http"])
			}
		}
	}
}

// ensureCredentials sets credentials to an empty map on v if unset.
func ensureCredentials(v any) {
	if m, ok := v.(map[string]any); ok {
		if _, has := m["credentials"]; !has {
			m["credentials"] = map[string]any{}
		}
	}
}

// rewriteMetadataURLs rewrites the scheme://host of every metadata callout URL to
// base, keeping each path. A malformed base or url is left untouched.
func rewriteMetadataURLs(spec map[string]any, base string) {
	b, err := neturl.Parse(base)
	if err != nil || b.Host == "" {
		return
	}
	meta, ok := spec["metadata"].(map[string]any)
	if !ok {
		return
	}
	for _, m := range meta {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		http, ok := mm["http"].(map[string]any)
		if !ok {
			continue
		}
		raw, ok := http["url"].(string)
		if !ok {
			continue
		}
		u, err := neturl.Parse(raw)
		if err != nil {
			continue
		}
		u.Scheme = b.Scheme
		u.Host = b.Host
		http["url"] = u.String()
	}
}

// authPolicyRules extracts defaults.rules from a buildGatewayAuthPolicySpec spec.
func authPolicyRules(authPolicySpec map[string]any) (map[string]any, bool) {
	defaults, ok := authPolicySpec["defaults"].(map[string]any)
	if !ok {
		return nil, false
	}
	rules, ok := defaults["rules"].(map[string]any)
	return rules, ok
}
