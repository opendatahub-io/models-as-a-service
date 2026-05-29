#!/bin/bash
#
# Create maas-default-gateway for Models-as-a-Service.
#
# Two deployment modes cover the full environment matrix:
#
# INGRESS_MODE     Service type            TLS source                         Best for
# -------------    ---------------------   --------------------------------   ------------------
# route (default)  Gateway-controller      Cluster router cert                ROSA, OSD, cloud
#                  default (LoadBalancer)  (auto-detected, four-level
#                                          fallback: IngressController →
#                                          router deployment → known
#                                          secrets → self-signed)
#
# clusterip        ClusterIP + OCP Route   service-ca-operator                On-prem,
#                  (reencrypt)             (auto-provisioned)                 disconnected,
#                                                                             bare-metal
#
# The clusterip mode applies all required resources in order:
#   ConfigMap/gw-options → Gateway → wait for Programmed → Route/maas-gateway-route
#
# The DISCONNECTED=true flag removes the GitHub fallback and fails fast if local
# manifests are missing.
#
# Environment variables:
#   INGRESS_MODE       "route" (default) or "clusterip"
#   DISCONNECTED       "true" to disable GitHub manifest fallback (default: false)
#   CLUSTER_DOMAIN     Override cluster domain auto-detection
#   CERT_NAME          Override TLS certificate secret name (route mode only)
#   DRY_RUN            "true" to preview without applying (default: false)
#   MAAS_MANIFEST_REF  Git tag or commit SHA for remote kustomize fallback (route mode)
#
# Usage:
#   # Route mode (ROSA, OSD, cloud)
#   ./scripts/setup-gateway.sh
#
#   # ClusterIP mode (on-prem, disconnected)
#   INGRESS_MODE=clusterip ./scripts/setup-gateway.sh
#
#   # Disconnected environment (no GitHub fetch)
#   DISCONNECTED=true INGRESS_MODE=clusterip ./scripts/setup-gateway.sh
#

set -euo pipefail

# Source helpers
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deployment-helpers.sh
source "${SCRIPT_DIR}/deployment-helpers.sh"

#──────────────────────────────────────────────────────────────
# CONFIGURATION
#──────────────────────────────────────────────────────────────

INGRESS_MODE="${INGRESS_MODE:-route}"
DISCONNECTED="${DISCONNECTED:-false}"
CLUSTER_DOMAIN="${CLUSTER_DOMAIN:-}"
CERT_NAME="${CERT_NAME:-}"
DRY_RUN="${DRY_RUN:-false}"
MAAS_MANIFEST_REF="${MAAS_MANIFEST_REF:-}"

# Constants
GATEWAY_NAMESPACE="openshift-ingress"
GATEWAY_NAME="maas-default-gateway"
GATEWAYCLASS_NAME="openshift-default"
GW_OPTIONS_CONFIGMAP="gw-options"
GATEWAY_ROUTE_NAME="maas-gateway-route"
SERVICE_CA_SECRET="maas-gw-service-tls"

# Gateway Service name follows the pattern: <gateway-name>-<gatewayclass-name>
GATEWAY_SERVICE_NAME="${GATEWAY_NAME}-${GATEWAYCLASS_NAME}"

# Timeout for Gateway Programmed condition (seconds)
GATEWAY_TIMEOUT="${CUSTOM_CHECK_TIMEOUT:-120}"

#──────────────────────────────────────────────────────────────
# VALIDATION
#──────────────────────────────────────────────────────────────

validate_configuration() {
  log_info "Validating gateway configuration..."

  # Validate ingress mode
  if [[ ! "$INGRESS_MODE" =~ ^(route|clusterip)$ ]]; then
    log_error "Invalid INGRESS_MODE: $INGRESS_MODE"
    log_error "Must be 'route' or 'clusterip'"
    exit 1
  fi

  # Validate required tools
  local missing=()
  command -v kubectl &>/dev/null || missing+=("kubectl")
  if [[ "$INGRESS_MODE" == "route" ]]; then
    command -v kustomize &>/dev/null || missing+=("kustomize")
    command -v envsubst &>/dev/null || missing+=("envsubst")
  fi
  if [[ ${#missing[@]} -gt 0 ]]; then
    log_error "Missing required tools:"
    for tool in "${missing[@]}"; do
      log_error "  - $tool"
    done
    exit 1
  fi

  log_info "Configuration validated"
  log_info "  Ingress mode: $INGRESS_MODE"
  log_info "  Disconnected: $DISCONNECTED"
}

#──────────────────────────────────────────────────────────────
# CLUSTER DOMAIN DETECTION
#──────────────────────────────────────────────────────────────

detect_cluster_domain() {
  if [[ -n "$CLUSTER_DOMAIN" ]]; then
    log_info "Using provided cluster domain: $CLUSTER_DOMAIN"
    return 0
  fi

  log_info "Detecting cluster domain..."
  CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>/dev/null || echo "")

  if [[ -z "$CLUSTER_DOMAIN" ]]; then
    log_error "Could not determine cluster domain"
    log_error "Set CLUSTER_DOMAIN environment variable manually"
    exit 1
  fi

  log_info "  Detected cluster domain: $CLUSTER_DOMAIN"
}

# Resolve immutable git ref for remote kustomize fallback (route mode only).
resolve_maas_manifest_ref() {
  if [[ -n "$MAAS_MANIFEST_REF" ]]; then
    echo "$MAAS_MANIFEST_REF"
    return 0
  fi

  local project_root
  if project_root="$(find_project_root 2>/dev/null)"; then
    local sha
    sha=$(git -C "$project_root" rev-parse HEAD 2>/dev/null || echo "")
    if [[ -n "$sha" ]]; then
      echo "$sha"
      return 0
    fi
  fi

  log_error "MAAS_MANIFEST_REF is not set and could not resolve from local git repository"
  log_error "Set MAAS_MANIFEST_REF to a release tag or commit SHA, use a full repo clone, or set DISCONNECTED=true"
  exit 1
}

#──────────────────────────────────────────────────────────────
# TLS CERTIFICATE DETECTION (route mode only)
#──────────────────────────────────────────────────────────────

detect_tls_certificate() {
  if [[ -n "$CERT_NAME" ]]; then
    log_info "Using provided TLS certificate: $CERT_NAME"
    return 0
  fi

  log_info "Detecting TLS certificate secret..."

  # Primary: Get certificate from IngressController (most reliable source)
  CERT_NAME=$(kubectl get ingresscontroller default -n openshift-ingress-operator \
    -o jsonpath='{.spec.defaultCertificate.name}' 2>/dev/null || echo "")

  if [[ -n "$CERT_NAME" ]] && kubectl get secret -n "$GATEWAY_NAMESPACE" "$CERT_NAME" &>/dev/null; then
    log_info "  Found certificate from IngressController: $CERT_NAME"
    return 0
  fi

  [[ -n "$CERT_NAME" ]] && log_debug "  IngressController cert '$CERT_NAME' not found, trying alternatives..."
  CERT_NAME=""

  # Fallback 1: Get certificate from router deployment
  CERT_NAME=$(kubectl get deployment router-default -n "$GATEWAY_NAMESPACE" \
    -o jsonpath='{.spec.template.spec.volumes[?(@.name=="default-certificate")].secret.secretName}' 2>/dev/null || echo "")

  if [[ -n "$CERT_NAME" ]] && kubectl get secret -n "$GATEWAY_NAMESPACE" "$CERT_NAME" &>/dev/null; then
    log_info "  Found certificate from router deployment: $CERT_NAME"
    return 0
  fi
  CERT_NAME=""

  # Fallback 2: Check known certificate secret names
  local cert_candidates=("default-gateway-cert" "router-certs-default")
  for cert in "${cert_candidates[@]}"; do
    if kubectl get secret -n "$GATEWAY_NAMESPACE" "$cert" &>/dev/null; then
      CERT_NAME="$cert"
      log_info "  Found TLS certificate secret: $CERT_NAME"
      return 0
    fi
  done

  # Final fallback: Create self-signed certificate
  log_warn "  No TLS certificate found. Creating self-signed certificate..."
  local gateway_hostname="maas.${CLUSTER_DOMAIN}"
  if [[ "$DRY_RUN" == "true" ]]; then
    CERT_NAME="maas-gateway-tls"
    log_info "  [DRY RUN] Would create self-signed certificate: $CERT_NAME"
    return 0
  fi
  if create_tls_secret "maas-gateway-tls" "$GATEWAY_NAMESPACE" "${gateway_hostname}"; then
    CERT_NAME="maas-gateway-tls"
    log_info "  Created self-signed certificate: $CERT_NAME"
  else
    log_error "Failed to create TLS certificate for gateway"
    exit 1
  fi
}

#──────────────────────────────────────────────────────────────
# GATEWAYCLASS SETUP
#──────────────────────────────────────────────────────────────

setup_gatewayclass() {
  log_info "Setting up GatewayClass..."

  # Note: Create-only; does not update existing GatewayClass.
  # Delete the GatewayClass to recreate if spec changes.
  if kubectl get gatewayclass "$GATEWAYCLASS_NAME" &>/dev/null; then
    log_info "  GatewayClass $GATEWAYCLASS_NAME already exists"
    return 0
  fi

  if [[ "$DRY_RUN" == "true" ]]; then
    log_info "  [DRY RUN] Would create GatewayClass $GATEWAYCLASS_NAME"
    return 0
  fi

  log_info "  Creating GatewayClass $GATEWAYCLASS_NAME..."

  kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: ${GATEWAYCLASS_NAME}
spec:
  controllerName: "openshift.io/gateway-controller/v1"
EOF
}

#──────────────────────────────────────────────────────────────
# ROUTE MODE SETUP
#──────────────────────────────────────────────────────────────

setup_route_mode() {
  log_info "Setting up Gateway in route mode..."

  detect_tls_certificate

  # Check if Gateway already exists and is Programmed
  if kubectl get gateway "$GATEWAY_NAME" -n "$GATEWAY_NAMESPACE" &>/dev/null; then
    log_info "  Gateway $GATEWAY_NAME already exists in $GATEWAY_NAMESPACE"

    if kubectl wait --for=condition=Programmed gateway/"$GATEWAY_NAME" -n "$GATEWAY_NAMESPACE" --timeout=10s &>/dev/null; then
      # Verify required annotations are present before skipping update
      local managed_anno authorino_anno params_ref
      managed_anno=$(kubectl get gateway "$GATEWAY_NAME" -n "$GATEWAY_NAMESPACE" \
        -o jsonpath='{.metadata.annotations.opendatahub\.io/managed}' 2>/dev/null || echo "")
      authorino_anno=$(kubectl get gateway "$GATEWAY_NAME" -n "$GATEWAY_NAMESPACE" \
        -o jsonpath='{.metadata.annotations.security\.opendatahub\.io/authorino-tls-bootstrap}' 2>/dev/null || echo "")
      # Route mode should NOT have infrastructure.parametersRef (clusterip mode does)
      params_ref=$(kubectl get gateway "$GATEWAY_NAME" -n "$GATEWAY_NAMESPACE" \
        -o jsonpath='{.spec.infrastructure.parametersRef.name}' 2>/dev/null || echo "")

      if [[ "$managed_anno" == "false" ]] && [[ "$authorino_anno" == "true" ]] && [[ -z "$params_ref" ]]; then
        log_info "  Gateway is Programmed with required annotations and route mode spec"
        return 0
      else
        if [[ -n "$params_ref" ]]; then
          log_info "  Gateway has parametersRef ($params_ref) - recreating for route mode..."
        else
          log_info "  Gateway is missing required annotations, updating..."
        fi
      fi
    else
      log_info "  Updating Gateway configuration..."
    fi
  fi

  if [[ "$DRY_RUN" == "true" ]]; then
    log_info "  [DRY RUN] Would create/update Gateway $GATEWAY_NAME"
    log_info "    Cluster domain: $CLUSTER_DOMAIN"
    log_info "    TLS certificate: $CERT_NAME"
    return 0
  fi

  log_info "  Creating/updating Gateway $GATEWAY_NAME..."
  log_info "    Cluster domain: $CLUSTER_DOMAIN"
  log_info "    TLS certificate: $CERT_NAME"

  # Determine manifest location
  local project_root
  project_root="$(find_project_root)" || {
    log_error "Could not determine project root"
    exit 1
  }

  local maas_networking_dir="$project_root/deployment/base/networking/maas"

  # Apply Gateway manifest with variable substitution
  if [[ -d "$maas_networking_dir" ]]; then
    log_debug "  Using local manifest: $maas_networking_dir"
    kustomize build "$maas_networking_dir" | \
      CLUSTER_DOMAIN="$CLUSTER_DOMAIN" CERT_NAME="$CERT_NAME" envsubst '$CLUSTER_DOMAIN $CERT_NAME' | \
      kubectl apply --server-side=true -f -
  elif [[ "$DISCONNECTED" == "true" ]]; then
    log_error "DISCONNECTED=true but local manifest not found: $maas_networking_dir"
    exit 1
  else
    # Fallback: fetch from GitHub (pinned ref, not mutable main)
    local manifest_ref
    manifest_ref="$(resolve_maas_manifest_ref)"
    log_debug "  Local manifest not found, fetching from GitHub (ref=${manifest_ref})..."
    kustomize build "https://github.com/opendatahub-io/models-as-a-service.git/deployment/base/networking/maas?ref=${manifest_ref}" | \
      CLUSTER_DOMAIN="$CLUSTER_DOMAIN" CERT_NAME="$CERT_NAME" envsubst '$CLUSTER_DOMAIN $CERT_NAME' | \
      kubectl apply --server-side=true -f -
  fi

  # Wait for Gateway to be Programmed
  # Note: In route mode, we warn but don't exit if the Gateway is not Programmed.
  # The Gateway resource is created successfully, and it may take longer on some
  # clusters (e.g., Service Mesh not fully ready). This is a soft check.
  # In clusterip mode, we exit 1 because the Gateway MUST be Programmed before
  # the Route can be created in the next step (see wait_gateway_programmed).
  log_info "  Waiting for Gateway to be Programmed (timeout: ${GATEWAY_TIMEOUT}s)..."
  if ! kubectl wait --for=condition=Programmed gateway/"$GATEWAY_NAME" -n "$GATEWAY_NAMESPACE" --timeout="${GATEWAY_TIMEOUT}s" 2>/dev/null; then
    log_warn "Gateway not Programmed after ${GATEWAY_TIMEOUT}s - check Service Mesh installation"
  else
    log_info "  Gateway is Programmed"
  fi
}

#──────────────────────────────────────────────────────────────
# CLUSTERIP MODE SETUP
#──────────────────────────────────────────────────────────────

setup_clusterip_mode() {
  log_info "Setting up Gateway in clusterip mode (with OpenShift Route)..."

  # Step 1: Create ConfigMap/gw-options
  create_gw_options_configmap

  # Step 2: Create Gateway with infrastructure.parametersRef
  create_clusterip_gateway

  # Step 3: Wait for Gateway to be Programmed (required before Route can work)
  wait_gateway_programmed

  # Step 4: Create OpenShift Route
  create_gateway_route

  log_info "ClusterIP mode setup complete"
}

create_gw_options_configmap() {
  # Note: Create-only; does not update existing ConfigMap content.
  # Same behavior as GatewayClass setup. Delete the ConfigMap to recreate.
  if kubectl get configmap "$GW_OPTIONS_CONFIGMAP" -n "$GATEWAY_NAMESPACE" &>/dev/null; then
    log_info "  ConfigMap $GW_OPTIONS_CONFIGMAP already exists"
    return 0
  fi

  if [[ "$DRY_RUN" == "true" ]]; then
    log_info "  [DRY RUN] Would create ConfigMap $GW_OPTIONS_CONFIGMAP"
    return 0
  fi

  log_info "  Creating ConfigMap $GW_OPTIONS_CONFIGMAP..."

  kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${GW_OPTIONS_CONFIGMAP}
  namespace: ${GATEWAY_NAMESPACE}
data:
  service: |
    metadata:
      annotations:
        # OpenShift service-ca-operator auto-provisions a TLS certificate into this Secret.
        # The Secret name here MUST match the certificateRefs in the Gateway listener.
        service.beta.openshift.io/serving-cert-secret-name: "${SERVICE_CA_SECRET}"
    spec:
      # ClusterIP avoids provisioning an external LoadBalancer; the OpenShift Route
      # handles external ingress instead.
      type: ClusterIP
EOF
}

create_clusterip_gateway() {
  # Check if Gateway already exists and is Programmed
  if kubectl get gateway "$GATEWAY_NAME" -n "$GATEWAY_NAMESPACE" &>/dev/null; then
    log_info "  Gateway $GATEWAY_NAME already exists in $GATEWAY_NAMESPACE"

    if kubectl wait --for=condition=Programmed gateway/"$GATEWAY_NAME" -n "$GATEWAY_NAMESPACE" --timeout=10s &>/dev/null; then
      # Verify required annotations AND spec match clusterip mode before skipping update
      local managed_anno authorino_anno params_ref
      managed_anno=$(kubectl get gateway "$GATEWAY_NAME" -n "$GATEWAY_NAMESPACE" \
        -o jsonpath='{.metadata.annotations.opendatahub\.io/managed}' 2>/dev/null || echo "")
      authorino_anno=$(kubectl get gateway "$GATEWAY_NAME" -n "$GATEWAY_NAMESPACE" \
        -o jsonpath='{.metadata.annotations.security\.opendatahub\.io/authorino-tls-bootstrap}' 2>/dev/null || echo "")
      # ClusterIP mode MUST have infrastructure.parametersRef pointing to gw-options ConfigMap
      params_ref=$(kubectl get gateway "$GATEWAY_NAME" -n "$GATEWAY_NAMESPACE" \
        -o jsonpath='{.spec.infrastructure.parametersRef.name}' 2>/dev/null || echo "")

      if [[ "$managed_anno" == "false" ]] && [[ "$authorino_anno" == "true" ]] && [[ "$params_ref" == "$GW_OPTIONS_CONFIGMAP" ]]; then
        log_info "  Gateway is Programmed with required annotations and clusterip mode spec"
        return 0
      else
        if [[ "$params_ref" != "$GW_OPTIONS_CONFIGMAP" ]]; then
          log_info "  Gateway spec doesn't match clusterip mode (parametersRef: '$params_ref' != '$GW_OPTIONS_CONFIGMAP') - recreating..."
        else
          log_info "  Gateway is missing required annotations, updating..."
        fi
      fi
    else
      log_info "  Updating Gateway configuration..."
    fi
  fi

  if [[ "$DRY_RUN" == "true" ]]; then
    log_info "  [DRY RUN] Would create/update Gateway $GATEWAY_NAME (clusterip mode)"
    return 0
  fi

  log_info "  Creating/updating Gateway $GATEWAY_NAME (clusterip mode)..."

  kubectl apply --server-side=true -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: ${GATEWAY_NAME}
  namespace: ${GATEWAY_NAMESPACE}
  labels:
    app.kubernetes.io/name: maas
    app.kubernetes.io/instance: ${GATEWAY_NAME}
    app.kubernetes.io/component: gateway
    opendatahub.io/managed: "false"
  annotations:
    opendatahub.io/managed: "false"
    security.opendatahub.io/authorino-tls-bootstrap: "true"
spec:
  gatewayClassName: ${GATEWAYCLASS_NAME}
  infrastructure:
    parametersRef:
      group: ""
      kind: ConfigMap
      name: ${GW_OPTIONS_CONFIGMAP}
  listeners:
    - name: https
      port: 443
      protocol: HTTPS
      allowedRoutes:
        namespaces:
          from: All
      tls:
        certificateRefs:
          - group: ""
            kind: Secret
            # Must match the serving-cert-secret-name in gw-options ConfigMap.
            # service-ca-operator populates this Secret automatically.
            name: ${SERVICE_CA_SECRET}
        mode: Terminate
EOF
}

wait_gateway_programmed() {
  if [[ "$DRY_RUN" == "true" ]]; then
    log_info "  [DRY RUN] Would wait for Gateway to be Programmed"
    return 0
  fi

  log_info "  Waiting for Gateway to be Programmed (timeout: ${GATEWAY_TIMEOUT}s)..."
  if ! kubectl wait --for=condition=Programmed gateway/"$GATEWAY_NAME" -n "$GATEWAY_NAMESPACE" --timeout="${GATEWAY_TIMEOUT}s" 2>/dev/null; then
    log_error "Gateway not Programmed after ${GATEWAY_TIMEOUT}s"
    log_error "Check Gateway status: kubectl get gateway $GATEWAY_NAME -n $GATEWAY_NAMESPACE -o yaml"
    exit 1
  fi

  log_info "  Gateway is Programmed"

  # Verify Gateway Service was created by controller
  if ! kubectl get svc "$GATEWAY_SERVICE_NAME" -n "$GATEWAY_NAMESPACE" &>/dev/null; then
    log_error "Gateway Service $GATEWAY_SERVICE_NAME not found"
    log_error "Expected Service name: $GATEWAY_SERVICE_NAME"
    log_error "Check available services: kubectl get svc -n $GATEWAY_NAMESPACE"
    exit 1
  fi

  log_info "  Gateway Service ready: $GATEWAY_SERVICE_NAME"
}

create_gateway_route() {
  if kubectl get route "$GATEWAY_ROUTE_NAME" -n "$GATEWAY_NAMESPACE" &>/dev/null; then
    local route_admitted
    route_admitted=$(kubectl get route "$GATEWAY_ROUTE_NAME" -n "$GATEWAY_NAMESPACE" \
      -o jsonpath='{.status.ingress[0].conditions[?(@.type=="Admitted")].status}' 2>/dev/null || echo "")

    if [[ "$route_admitted" == "True" ]]; then
      log_info "  Route $GATEWAY_ROUTE_NAME already exists and is Admitted"
      return 0
    fi
  fi

  if [[ "$DRY_RUN" == "true" ]]; then
    log_info "  [DRY RUN] Would create Route $GATEWAY_ROUTE_NAME"
    log_info "    Host: maas.$CLUSTER_DOMAIN"
    log_info "    Target Service: $GATEWAY_SERVICE_NAME"
    return 0
  fi

  log_info "  Creating Route $GATEWAY_ROUTE_NAME..."
  log_info "    Host: maas.$CLUSTER_DOMAIN"
  log_info "    Target Service: $GATEWAY_SERVICE_NAME"

  # Fetch the service CA bundle so the router can verify the backend certificate
  local service_ca_bundle
  service_ca_bundle=$(kubectl get configmap signing-cabundle -n openshift-service-ca \
    -o jsonpath='{.data.ca-bundle\.crt}' 2>/dev/null || echo "")

  if [[ -z "$service_ca_bundle" ]]; then
    log_error "Failed to retrieve service CA bundle from openshift-service-ca/signing-cabundle"
    log_error "The Route reencrypt termination requires this CA to verify the backend certificate"
    exit 1
  fi

  kubectl apply -f - <<EOF
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: ${GATEWAY_ROUTE_NAME}
  namespace: ${GATEWAY_NAMESPACE}
spec:
  host: maas.${CLUSTER_DOMAIN}
  to:
    kind: Service
    # The Gateway controller creates a Service whose name is derived from the
    # Gateway name and GatewayClass name.
    name: ${GATEWAY_SERVICE_NAME}
    weight: 100
  port:
    targetPort: 443
  tls:
    # Re-encrypt: the Router terminates the client TLS session and opens a new
    # TLS connection to the Gateway Service using the service-ca certificate.
    termination: reencrypt
    insecureEdgeTerminationPolicy: Redirect
    # Provide the service CA bundle so the router can verify the backend certificate.
    # Without this, reencrypt handshake will fail on clusters where the router
    # doesn't implicitly trust service-ca (e.g., bare-metal, disconnected).
    destinationCACertificate: |
$(echo "$service_ca_bundle" | sed 's/^/      /')
EOF

  # Wait briefly for Route to be Admitted
  log_info "  Waiting for Route to be Admitted..."
  local wait_time=0
  local max_wait=30
  while [[ $wait_time -lt $max_wait ]]; do
    local route_admitted
    route_admitted=$(kubectl get route "$GATEWAY_ROUTE_NAME" -n "$GATEWAY_NAMESPACE" \
      -o jsonpath='{.status.ingress[0].conditions[?(@.type=="Admitted")].status}' 2>/dev/null || echo "")

    if [[ "$route_admitted" == "True" ]]; then
      log_info "  Route is Admitted"
      return 0
    fi

    sleep 2
    wait_time=$((wait_time + 2))
  done

  log_warn "Route not Admitted after ${max_wait}s - check Route status manually"
}

#──────────────────────────────────────────────────────────────
# MAIN ENTRY POINT
#──────────────────────────────────────────────────────────────

main() {
  log_info "==================================================="
  log_info "  MaaS Gateway Setup"
  log_info "==================================================="

  validate_configuration
  detect_cluster_domain

  # Setup GatewayClass (required for both modes)
  setup_gatewayclass

  # Mode-specific setup
  case "$INGRESS_MODE" in
    route)
      setup_route_mode
      ;;
    clusterip)
      setup_clusterip_mode
      ;;
  esac

  if [[ "$DRY_RUN" == "true" ]]; then
    log_info ""
    log_info "DRY RUN MODE - no changes were applied"
  fi

  log_info ""
  log_info "==================================================="
  log_info "  Gateway setup completed successfully"
  log_info "==================================================="
  log_info "  Mode: $INGRESS_MODE"
  log_info "  Gateway: $GATEWAY_NAME"
  log_info "  Namespace: $GATEWAY_NAMESPACE"

  if [[ "$INGRESS_MODE" == "clusterip" ]]; then
    log_info "  Route: $GATEWAY_ROUTE_NAME"
    log_info "  Hostname: maas.$CLUSTER_DOMAIN"
  fi

  log_info ""
  log_info "Verify with:"
  log_info "  kubectl get gateway $GATEWAY_NAME -n $GATEWAY_NAMESPACE"

  if [[ "$INGRESS_MODE" == "clusterip" ]]; then
    log_info "  kubectl get route $GATEWAY_ROUTE_NAME -n $GATEWAY_NAMESPACE"
  fi
}

main "$@"
