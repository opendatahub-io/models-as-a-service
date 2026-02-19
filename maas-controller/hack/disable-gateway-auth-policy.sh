#!/usr/bin/env bash
# Disables the gateway-auth-policy in openshift-ingress so the MaaS controller
# can manage auth via MaaSAuthPolicy per HTTPRoute. The policy cannot be removed
# because it is controlled by another operator; we annotate it as unmanaged and
# point its targetRef to a non-existing gateway so it no longer applies.
#
# Usage: ./hack/disable-gateway-auth-policy.sh

set -euo pipefail

NAMESPACE="${NAMESPACE:-openshift-ingress}"
POLICY_NAME="${POLICY_NAME:-gateway-auth-policy}"
# Non-existing gateway name so the policy does not bind to the real gateway
DISABLED_GATEWAY_NAME="${DISABLED_GATEWAY_NAME:-maas-gateway-disabled-by-maas-controller}"

if ! kubectl get authpolicies.kuadrant.io -n "$NAMESPACE" "$POLICY_NAME" &>/dev/null; then
  echo "AuthPolicy $POLICY_NAME not found in $NAMESPACE (may already be disabled or not installed). Skipping."
  exit 0
fi

echo "Disabling gateway-auth-policy in $NAMESPACE (annotate managed=false, point to non-existing gateway)..."

# Mark as not managed by ODH so the other operator does not reconcile it back
kubectl annotate authpolicies.kuadrant.io "$POLICY_NAME" -n "$NAMESPACE" \
  opendatahub.io/managed=false \
  --overwrite

# Point targetRef to a non-existing gateway so this policy no longer applies to maas-default-gateway
kubectl patch authpolicies.kuadrant.io "$POLICY_NAME" -n "$NAMESPACE" --type=merge -p '{"spec":{"targetRef":{"name":"'"$DISABLED_GATEWAY_NAME"'"}}}'

echo "Done. AuthPolicy $POLICY_NAME now targets non-existing gateway $DISABLED_GATEWAY_NAME and is annotated opendatahub.io/managed=false."
