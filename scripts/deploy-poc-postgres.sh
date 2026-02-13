#!/bin/bash
# deploy-poc-postgres.sh
# Quick PostgreSQL setup for API Key Management POC
# NOT for production use

set -e

NAMESPACE="${NAMESPACE:-maas-system}"
POSTGRES_USER="${POSTGRES_USER:-maas}"
POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-pocpassword}"
POSTGRES_DB="${POSTGRES_DB:-maas_db}"

echo "=== Deploying POC PostgreSQL to namespace: $NAMESPACE ==="

# Create namespace if it doesn't exist
kubectl get namespace "$NAMESPACE" &>/dev/null || kubectl create namespace "$NAMESPACE"

# Deploy PostgreSQL
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
        image: postgres:15-alpine
        envFrom:
        - secretRef:
            name: postgres-creds
        ports:
        - containerPort: 5432
        resources:
          requests:
            memory: "256Mi"
            cpu: "100m"
          limits:
            memory: "512Mi"
            cpu: "500m"
        readinessProbe:
          exec:
            command: ["pg_isready", "-U", "${POSTGRES_USER}"]
          initialDelaySeconds: 5
          periodSeconds: 5
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
---
apiVersion: v1
kind: Secret
metadata:
  name: maas-db-config
  labels:
    app: maas-api
    purpose: poc
stringData:
  DB_CONNECTION_URL: "postgresql://${POSTGRES_USER}:${POSTGRES_PASSWORD}@postgres:5432/${POSTGRES_DB}?sslmode=disable"
EOF

echo "Waiting for PostgreSQL to be ready..."
kubectl wait -n "$NAMESPACE" --for=condition=available deployment/postgres --timeout=120s

echo ""
echo "=== POC PostgreSQL Deployed ==="
echo ""
echo "Connection details:"
echo "  Namespace: $NAMESPACE"
echo "  Service:   postgres:5432"
echo "  Database:  $POSTGRES_DB"
echo "  User:      $POSTGRES_USER"
echo ""
echo "For maas-api, use secret 'maas-db-config' with key 'DB_CONNECTION_URL'"
echo ""
echo "Example maas-api deployment snippet:"
echo "  env:"
echo "  - name: DB_CONNECTION_URL"
echo "    valueFrom:"
echo "      secretKeyRef:"
echo "        name: maas-db-config"
echo "        key: DB_CONNECTION_URL"
echo ""
echo "To cleanup: kubectl delete -n $NAMESPACE deploy/postgres svc/postgres secret/postgres-creds secret/maas-db-config"
