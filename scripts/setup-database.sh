#!/bin/bash
#
# Deploy PostgreSQL for MaaS API key storage.
#
# Creates a PostgreSQL Deployment, Service, and the maas-db-config Secret
# containing DB_CONNECTION_URL. This is a POC-grade setup with ephemeral
# storage; for production use AWS RDS, Crunchy Operator, or Azure Database.
#
# Namespace selection:
#   - Use NAMESPACE environment variable if set
#   - Default: opendatahub (ODH) or redhat-ods-applications (RHOAI)
#
# Environment variables:
#   NAMESPACE       Target namespace (default: opendatahub)
#   POSTGRES_USER   Database user (default: maas)
#   POSTGRES_DB     Database name (default: maas)
#   POSTGRES_PASSWORD  Database password (default: auto-generated)
#
# Usage:
#   ./scripts/setup-database.sh
#   NAMESPACE=redhat-ods-applications ./scripts/setup-database.sh
#
# Docker alternative: Replace 'kubectl' with 'oc' if using OpenShift.
#

set -euo pipefail

# Source helpers
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deployment-helpers.sh
source "${SCRIPT_DIR}/deployment-helpers.sh"

# Default namespace for ODH; use redhat-ods-applications for RHOAI
: "${NAMESPACE:=opendatahub}"

# Ensure namespace exists
if ! kubectl get namespace "$NAMESPACE" >/dev/null 2>&1; then
  echo "📦 Creating namespace '$NAMESPACE'..."
  kubectl create namespace "$NAMESPACE"
fi

echo ""
echo "┏━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┓"
echo "┃  ⚠️  WARNING FOR PRODUCTION USE. ⚠️                             ┃"
echo "┃  This deploys PostgreSQL with ephemeral storage (emptyDir).     ┃"
echo "┃  Data WILL be lost on pod restart.                              ┃"
echo "┃  For production, use an external database:                      ┃"
echo "┃    deploy.sh --postgres-connection postgresql://...             ┃"
echo "┗━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┛"
echo ""
echo "🔧 Deploying PostgreSQL for API key storage in namespace '$NAMESPACE'..."

# Check if PostgreSQL already exists
if kubectl get deployment postgres -n "$NAMESPACE" &>/dev/null; then
  echo "  PostgreSQL already deployed in namespace $NAMESPACE"
  echo "  Service: postgres:5432"
  echo "  Secret: maas-db-config (contains DB_CONNECTION_URL)"
  exit 0
fi

# PostgreSQL configuration (POC-grade, not for production)
POSTGRES_USER="${POSTGRES_USER:-maas}"
POSTGRES_DB="${POSTGRES_DB:-maas}"

# Generate random password if not provided
if [[ -z "${POSTGRES_PASSWORD:-}" ]]; then
  POSTGRES_PASSWORD="$(openssl rand -base64 32 | tr -d '/+=' | cut -c1-32)"
  echo "  Generated random PostgreSQL password (stored in secret postgres-creds)"
fi

echo "  Creating PostgreSQL deployment..."
echo "  ⚠️  Using POC configuration (ephemeral storage)"
echo ""

# Deploy PostgreSQL resources
kubectl apply -n "$NAMESPACE" -f - <<EOF
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
        image: registry.redhat.io/rhel9/postgresql-15:latest
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
DB_CONNECTION_URL="postgresql://${POSTGRES_USER}:${ENCODED_PASSWORD}@postgres:5432/${POSTGRES_DB}?sslmode=disable"
create_maas_db_config_secret "$NAMESPACE" "$DB_CONNECTION_URL"

echo "  Waiting for PostgreSQL to be ready..."
if ! kubectl wait -n "$NAMESPACE" --for=condition=available deployment/postgres --timeout=120s; then
  echo "❌ PostgreSQL deployment failed to become ready" >&2
  exit 1
fi

echo ""
echo "✅ PostgreSQL deployed successfully"
echo "  Database: $POSTGRES_DB"
echo "  User: $POSTGRES_USER"
echo "  Secret: maas-db-config (contains DB_CONNECTION_URL)"
echo ""
echo "  ⚠️  For production, use AWS RDS, Crunchy Operator, or Azure Database"
echo "  Note: Schema migrations run automatically when maas-api starts"
