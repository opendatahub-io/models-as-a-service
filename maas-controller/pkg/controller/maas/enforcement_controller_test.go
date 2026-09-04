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
	"testing"

	"sigs.k8s.io/yaml"
)

func TestRenderPluginConfig(t *testing.T) {
	r := &GatewayEnforcementReconciler{
		AuthUpstream:      "https://authorino:50051",
		RatelimitUpstream: "http://limitador:8081",
		AuthCACertPath:    "/etc/kuadrant-tls/service-ca.crt",
		AuthSNI:           "authorino.svc",
	}
	pc := PluginConfiguration{
		Services:   standardServices(),
		ActionSets: []ActionSet{{Name: "x", RouteRuleConditions: RouteRuleConditions{Hostnames: []string{"h"}}}},
	}

	rendered, err := r.renderPluginConfig(pc)
	if err != nil {
		t.Fatalf("renderPluginConfig: %v", err)
	}

	var doc struct {
		Plugin    map[string]any               `json:"plugin"`
		Upstreams map[string]string            `json:"upstreams"`
		TLS       map[string]map[string]string `json:"tls"`
	}
	if err := yaml.Unmarshal([]byte(rendered), &doc); err != nil {
		t.Fatalf("rendered config does not parse: %v\n%s", err, rendered)
	}
	if _, ok := doc.Plugin["actionSets"]; !ok {
		t.Errorf("plugin.actionSets missing")
	}
	if doc.Upstreams[authServiceEndpoint] != r.AuthUpstream {
		t.Errorf("auth upstream = %q, want %q", doc.Upstreams[authServiceEndpoint], r.AuthUpstream)
	}
	if doc.Upstreams[ratelimitServiceEndpoint] != r.RatelimitUpstream {
		t.Errorf("ratelimit upstream = %q", doc.Upstreams[ratelimitServiceEndpoint])
	}
	if tls := doc.TLS[authServiceEndpoint]; tls["ca_cert"] != r.AuthCACertPath || tls["sni"] != r.AuthSNI {
		t.Errorf("auth tls = %v", tls)
	}
}

func TestConfigMapForGateway(t *testing.T) {
	cm := configMapFor("maas-site-a-gateway", "openshift-ingress", "plugin: {}\n")

	if cm.Name != "maas-site-a-gateway-extproc" {
		t.Errorf("ConfigMap name = %q", cm.Name)
	}
	if cm.Namespace != "openshift-ingress" {
		t.Errorf("ConfigMap namespace = %q", cm.Namespace)
	}
	if _, ok := cm.Data["kuadrant.yaml"]; !ok {
		t.Errorf("ConfigMap missing kuadrant.yaml key")
	}
}
