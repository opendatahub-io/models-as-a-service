#!/bin/bash
#
# Deploy PostgreSQL for MaaS API key storage.
#
# Creates a PostgreSQL Deployment, Service, and the maas-db-config Secret
# containing DB_CONNECTION_URL. This is a POC-grade setup with ephemeral
# storage; for production use AWS RDS, Crunchy Operator, or Azure Database.
#
# Namespace selection:
#   - Auto-detects upgrades vs fresh installs
#   - Upgrades: Keeps postgres in legacy namespace (opendatahub/redhat-ods-applications),
#               copies maas-db-config secret to infrastructure namespace (controlled by INFRA_NAMESPACE)
#   - Fresh installs: Creates postgres in infrastructure namespace (controlled by INFRA_NAMESPACE)
#
# Environment variables:
#   POSTGRES_USER      Database user (default: maas)
#   POSTGRES_DB        Database name (default: maas)
#   POSTGRES_PASSWORD  Database password (default: auto-generated)
#   DB_SSLMODE         PostgreSQL sslmode (default: require). Set to "disable"
#                      for CI environments where the Postgres pod lacks TLS.
#
# Usage:
#   ./scripts/setup-database.sh
#
# Docker alternative: Replace 'kubectl' with 'oc' if using OpenShift.
#

set -euo pipefail

# Source helpers
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deployment-helpers.sh
source "${SCRIPT_DIR}/deployment-helpers.sh"

# Derive infrastructure namespace from controller namespace (matches Go code logic)
derive_infra_namespace() {
  local controller_ns="$1"
  case "$controller_ns" in
    redhat-ods-applications)
      echo "redhat-ai-gateway-infra"
      ;;
    opendatahub)
      echo "odh-ai-gateway-infra"
      ;;
    *)
      echo "$controller_ns"
      ;;
  esac
}

# Infrastructure namespace where maas-api and postgres deploy
# Defaults to AUTO (namespace separation enabled). Set to empty string to disable for ROSA.
INFRA_NAMESPACE_RAW="${INFRA_NAMESPACE:-AUTO}"
if [ "$INFRA_NAMESPACE_RAW" = "AUTO" ]; then
  # Derive from NAMESPACE (controller namespace) or default to opendatahub
  CONTROLLER_NS="${NAMESPACE:-opendatahub}"
  INFRA_NAMESPACE=$(derive_infra_namespace "$CONTROLLER_NS")
else
  INFRA_NAMESPACE="$INFRA_NAMESPACE_RAW"
fi

# Legacy namespaces to check for existing postgres (upgrade detection)
LEGACY_NAMESPACES=("opendatahub" "redhat-ods-applications")

# Fallback image when the RHOAI operator CSV is not available (e.g., vanilla
# Kubernetes, ODH-only clusters, or dev environments without OLM).
DEFAULT_POSTGRES_IMAGE="registry.redhat.io/rhel9/postgresql-16:latest"

# Resolve the PostgreSQL image from the RHOAI operator CSV's relatedImages.
# Expects the RHOAI operator (or its CSV) to be installed on the cluster.
# Falls back to DEFAULT_POSTGRES_IMAGE if the CSV is not found or the entry
# is missing.
resolve_postgres_image() {
  local csv_image
  csv_image=$(kubectl get csv -l 'olm.copiedFrom=redhat-ods-operator' \
    -o jsonpath='{.items[0].spec.relatedImages[?(@.name=="postgresql_16_image")].image}' 2>/dev/null) || true

  if [[ -n "${csv_image}" ]]; then
    echo "${csv_image}"
  else
    echo "${DEFAULT_POSTGRES_IMAGE}"
  fi
}

# Detect upgrade vs fresh install by checking for existing postgres in legacy namespaces or infrastructure namespace
EXISTING_POSTGRES_NS=""
# Check infrastructure namespace first (current location)
if kubectl get deployment postgres -n "$INFRA_NAMESPACE" &>/dev/null 2>&1; then
  EXISTING_POSTGRES_NS="$INFRA_NAMESPACE"
else
  # Fall back to checking legacy namespaces for upgrade scenario
  for ns in "${LEGACY_NAMESPACES[@]}"; do
    if kubectl get deployment postgres -n "$ns" &>/dev/null 2>&1; then
      EXISTING_POSTGRES_NS="$ns"
      break
    fi
  done
fi

if [[ -n "$EXISTING_POSTGRES_NS" ]]; then
  # UPGRADE PATH: postgres exists in legacy namespace
  echo ""
  echo "🔄 Detected existing PostgreSQL in namespace '$EXISTING_POSTGRES_NS'"
  echo "  Upgrade mode: Keeping postgres in place, ensuring secret exists in infrastructure namespace"
  echo ""

  # Get existing connection URL and update to use FQDN
  if ! kubectl get secret maas-db-config -n "$EXISTING_POSTGRES_NS" &>/dev/null; then
    echo "❌ Error: postgres exists but maas-db-config secret not found in $EXISTING_POSTGRES_NS" >&2
    exit 1
  fi

  EXISTING_URL=$(kubectl get secret maas-db-config -n "$EXISTING_POSTGRES_NS" -o jsonpath='{.data.DB_CONNECTION_URL}' | base64 -d)

  # Replace short hostname with FQDN for cross-namespace access.
  # Extract the service name from the connection URL and append the namespace FQDN.
  # Handles URLs like: postgresql://user:pass@postgres:5432/db or @postgres-primary:5432/db
  # Only append FQDN if the hostname doesn't already contain dots (not already a FQDN)
  FQDN_URL=$(echo "$EXISTING_URL" | sed -E "s|@([^.:/@]+)(:[0-9]+)|@\1.${EXISTING_POSTGRES_NS}.svc.cluster.local\2|")

  # Ensure infrastructure namespace exists
  if ! kubectl get namespace "$INFRA_NAMESPACE" >/dev/null 2>&1; then
    echo "📦 Creating infrastructure namespace '$INFRA_NAMESPACE'..."
    kubectl create namespace "$INFRA_NAMESPACE"
  fi

  # Create/update secret in infrastructure namespace with FQDN
  echo "  Creating maas-db-config secret in '$INFRA_NAMESPACE' with FQDN connection string"
  create_maas_db_config_secret "$INFRA_NAMESPACE" "$FQDN_URL"

  echo ""
  echo "✅ Upgrade complete"
  echo "  PostgreSQL: $EXISTING_POSTGRES_NS/postgres (unchanged)"
  echo "  Secret: $INFRA_NAMESPACE/maas-db-config (FQDN connection)"
  echo ""
  exit 0
fi

# FRESH INSTALL PATH: No existing postgres found
echo ""
echo "┏━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┓"
echo "┃  ⚠️  WARNING FOR PRODUCTION USE. ⚠️                             ┃"
echo "┃  This deploys PostgreSQL with ephemeral storage (emptyDir).     ┃"
echo "┃  Data WILL be lost on pod restart.                              ┃"
echo "┃  For production, use an external database:                      ┃"
echo "┃    deploy.sh --postgres-connection postgresql://...             ┃"
echo "┗━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┛"
echo ""
echo "🔧 Fresh install: Deploying PostgreSQL in infrastructure namespace '$INFRA_NAMESPACE'..."

# Ensure infrastructure namespace exists
if ! kubectl get namespace "$INFRA_NAMESPACE" >/dev/null 2>&1; then
  echo "📦 Creating infrastructure namespace '$INFRA_NAMESPACE'..."
  kubectl create namespace "$INFRA_NAMESPACE"
fi

# PostgreSQL configuration (POC-grade, not for production)
POSTGRES_USER="${POSTGRES_USER:-maas}"
POSTGRES_DB="${POSTGRES_DB:-maas}"

# Generate random password if not provided and secret doesn't already exist
if [[ -z "${POSTGRES_PASSWORD:-}" ]]; then
  # Check if postgres-creds secret already exists (from previous run)
  if kubectl get secret postgres-creds -n "$INFRA_NAMESPACE" &>/dev/null; then
    echo "  Using existing postgres-creds secret (password preserved from previous deployment)"
    POSTGRES_PASSWORD="$(kubectl get secret postgres-creds -n "$INFRA_NAMESPACE" -o jsonpath='{.data.POSTGRES_PASSWORD}' | base64 -d)"
  else
    POSTGRES_PASSWORD="$(openssl rand -base64 32 | tr -d '/+=' | cut -c1-32)"
    echo "  Generated random PostgreSQL password (stored in secret postgres-creds)"
  fi
fi

echo "  Creating PostgreSQL deployment..."
echo "  ⚠️  Using POC configuration (ephemeral storage)"

POSTGRES_IMAGE="$(resolve_postgres_image)"
if [[ "${POSTGRES_IMAGE}" == "${DEFAULT_POSTGRES_IMAGE}" ]]; then
  echo "  Using default PostgreSQL image (operator CSV not available)"
else
  echo "  Resolved PostgreSQL image from operator CSV"
fi
echo "  Image: ${POSTGRES_IMAGE}"
echo ""

# Deploy PostgreSQL resources
kubectl apply -n "$INFRA_NAMESPACE" -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: postgres-creds
  labels:
    app: postgres
    purpose: poc
stringData:
  POSTGRES_USER: "${POSTGRES_USER}"
  POSTGRES_PASSWORD: "${POSTGRES_PASSWORD}"
  POSTGRES_DB: "${POSTGRES_DB}"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
  labels:
    app: postgres
    purpose: poc
spec:
  replicas: 1
  selector:
    matchLabels:
      app: postgres
  template:
    metadata:
      labels:
        app: postgres
    spec:
      containers:
      - name: postgres
        image: "${POSTGRES_IMAGE}"
        env:
        - name: POSTGRESQL_USER
          valueFrom:
            secretKeyRef:
              name: postgres-creds
              key: POSTGRES_USER
        - name: POSTGRESQL_PASSWORD
          valueFrom:
            secretKeyRef:
              name: postgres-creds
              key: POSTGRES_PASSWORD
        - name: POSTGRESQL_DATABASE
          valueFrom:
            secretKeyRef:
              name: postgres-creds
              key: POSTGRES_DB
        ports:
        - containerPort: 5432
        volumeMounts:
        - name: data
          mountPath: /var/lib/pgsql/data
        resources:
          requests:
            memory: "256Mi"
            cpu: "100m"
          limits:
            memory: "512Mi"
            cpu: "500m"
        readinessProbe:
          exec:
            command: ["/usr/libexec/check-container"]
          initialDelaySeconds: 5
          periodSeconds: 5
      volumes:
      - name: data
        emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: postgres
  labels:
    app: postgres
    purpose: poc
spec:
  selector:
    app: postgres
  ports:
  - port: 5432
    targetPort: 5432
EOF

# Create the maas-db-config secret used by maas-api
# URL-encode the password in case it contains reserved characters (@, :, /, ?, etc.)
# 1. printf '%s' outputs the raw password bytes (no trailing newline)
# 2. od -An -tx1 converts each byte to space-separated two-digit hex (e.g., "a" -> " 61")
# 3. tr -d ' \n' strips spaces and newlines to produce a continuous hex string
# 4. sed inserts a "%" before every hex pair, producing percent-encoding (e.g., "61" -> "%61")
# This encodes all characters (including safe ones like letters), which is more aggressive
# than strictly necessary but is always correct per RFC 3986 — %61 is equivalent to "a".
# Uses od (POSIX) instead of xxd which may not be available in all environments.
ENCODED_PASSWORD=$(printf '%s' "$POSTGRES_PASSWORD" | od -An -tx1 | tr -d ' \n' | sed 's/../%&/g')
: "${DB_SSLMODE:=require}"
DB_CONNECTION_URL="postgresql://${POSTGRES_USER}:${ENCODED_PASSWORD}@postgres:5432/${POSTGRES_DB}?sslmode=${DB_SSLMODE}"
create_maas_db_config_secret "$INFRA_NAMESPACE" "$DB_CONNECTION_URL"

echo "  Waiting for PostgreSQL to be ready..."
if ! kubectl wait -n "$INFRA_NAMESPACE" --for=condition=available deployment/postgres --timeout=120s; then
  echo "❌ PostgreSQL deployment failed to become ready" >&2
  exit 1
fi

echo ""
echo "✅ PostgreSQL deployed successfully"
echo "  Namespace: $INFRA_NAMESPACE"
echo "  Database: $POSTGRES_DB"
echo "  User: $POSTGRES_USER"
echo "  Secret: maas-db-config (contains DB_CONNECTION_URL with FQDN)"
echo ""
echo "  ⚠️  For production, use AWS RDS, Crunchy Operator, or Azure Database"
echo "  Note: Schema migrations run automatically when maas-api starts"
