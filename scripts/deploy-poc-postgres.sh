#!/bin/bash
# deploy-poc-postgres.sh
# Quick PostgreSQL setup for API Key Management POC
# NOT for production use
#
# Schema migrations are handled automatically by maas-api on startup.

set -e

NAMESPACE="${NAMESPACE:-maas-system}"
POSTGRES_USER="${POSTGRES_USER:-maas}"
POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-maaspassword}"
POSTGRES_DB="${POSTGRES_DB:-maas}"

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
echo "Note: Schema migrations are applied automatically by maas-api on startup."
echo ""
echo "For maas-api deployment, use secret 'maas-db-config':"
echo "  env:"
echo "  - name: DB_CONNECTION_URL"
echo "    valueFrom:"
echo "      secretKeyRef:"
echo "        name: maas-db-config"
echo "        key: DB_CONNECTION_URL"
echo ""
echo "To test locally with port-forward:"
echo "  kubectl port-forward -n $NAMESPACE svc/postgres 5432:5432"
echo "  export DB_CONNECTION_URL=\"postgresql://$POSTGRES_USER:$POSTGRES_PASSWORD@localhost:5432/$POSTGRES_DB?sslmode=disable\""
echo ""
echo "To cleanup: kubectl delete -n $NAMESPACE deploy/postgres svc/postgres secret/postgres-creds secret/maas-db-config"
