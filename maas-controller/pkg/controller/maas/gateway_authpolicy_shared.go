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
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// gatewayAuthPolicyParams holds the parameters needed to reconcile a gateway AuthPolicy.
type gatewayAuthPolicyParams struct {
	Client           client.Client
	GatewayNamespace string
	GatewayName      string
	TenantID         string
	ModelAccessJSON  string
	OIDCConfig       *oidcConfig
	MetadataCacheTTL int64
	AuthPolicyName   string // Optional, defaults to "{gatewayName}-maas-auth"
}

// reconcileGatewayAuthPolicyShared is the shared implementation of gateway AuthPolicy reconciliation.
// Both MaaSAuthPolicyReconciler and TenantReconciler call this function.
func reconcileGatewayAuthPolicyShared(ctx context.Context, log logr.Logger, params gatewayAuthPolicyParams) error {
	log.Info("reconciling gateway AuthPolicy",
		"gatewayNamespace", params.GatewayNamespace,
		"gatewayName", params.GatewayName,
		"tenantID", params.TenantID)

	spec := buildGatewayAuthPolicySpecShared(params.ModelAccessJSON, params.OIDCConfig, params.TenantID, params.MetadataCacheTTL)

	authPolicyName := params.AuthPolicyName
	if authPolicyName == "" {
		authPolicyName = fmt.Sprintf("%s-maas-auth", params.GatewayName)
	}

	gwPolicy := &unstructured.Unstructured{}
	gwPolicy.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	gwPolicy.SetName(authPolicyName)
	gwPolicy.SetNamespace(params.GatewayNamespace)

	_, err := controllerutil.CreateOrUpdate(ctx, params.Client, gwPolicy, func() error {
		if err := unstructured.SetNestedField(gwPolicy.Object, spec, "spec"); err != nil {
			return fmt.Errorf("failed to set gateway AuthPolicy spec: %w", err)
		}
		if err := unstructured.SetNestedMap(gwPolicy.Object, map[string]any{
			"gateway.networking.k8s.io/gateway-name": params.GatewayName,
		}, "spec", "targetRef"); err != nil {
			return fmt.Errorf("failed to set targetRef: %w", err)
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to reconcile gateway AuthPolicy %s/%s: %w", params.GatewayNamespace, authPolicyName, err)
	}

	log.Info("gateway AuthPolicy reconciled successfully", "name", authPolicyName)
	return nil
}

// buildGatewayAuthPolicySpecShared builds the AuthPolicy spec.
// Extracted from MaaSAuthPolicyReconciler.buildGatewayAuthPolicySpec.
func buildGatewayAuthPolicySpecShared(modelAccessJSON string, oidc *oidcConfig, tenantID string, metadataCacheTTL int64) map[string]any {
	// This will be the extracted implementation from MaaSAuthPolicyReconciler.buildGatewayAuthPolicySpec
	// For now, this is a placeholder that we'll fill in with the actual implementation
	return map[string]any{}
}
