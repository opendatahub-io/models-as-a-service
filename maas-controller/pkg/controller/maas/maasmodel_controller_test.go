/*
Copyright 2025.

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
)

func TestMaaSModelReconciler_gatewayName(t *testing.T) {
	t.Run("default_when_empty", func(t *testing.T) {
		r := &MaaSModelReconciler{}
		if got := r.gatewayName(); got != defaultGatewayName {
			t.Errorf("gatewayName() = %q, want %q", got, defaultGatewayName)
		}
	})
	t.Run("custom_when_set", func(t *testing.T) {
		r := &MaaSModelReconciler{GatewayName: "my-gateway"}
		if got := r.gatewayName(); got != "my-gateway" {
			t.Errorf("gatewayName() = %q, want %q", got, "my-gateway")
		}
	})
}

func TestMaaSModelReconciler_gatewayNamespace(t *testing.T) {
	t.Run("default_when_empty", func(t *testing.T) {
		r := &MaaSModelReconciler{}
		if got := r.gatewayNamespace(); got != defaultGatewayNamespace {
			t.Errorf("gatewayNamespace() = %q, want %q", got, defaultGatewayNamespace)
		}
	})
	t.Run("custom_when_set", func(t *testing.T) {
		r := &MaaSModelReconciler{GatewayNamespace: "my-ns"}
		if got := r.gatewayNamespace(); got != "my-ns" {
			t.Errorf("gatewayNamespace() = %q, want %q", got, "my-ns")
		}
	})
}
