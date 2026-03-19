#!/bin/bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DATA_DIR="${SCRIPT_DIR%/installers}/data"

KEYCLOAK_NAMESPACE="${KEYCLOAK_NAMESPACE:-keycloak}"
KEYCLOAK_NAME="${KEYCLOAK_NAME:-maas-keycloak}"
KEYCLOAK_IMAGE="${KEYCLOAK_IMAGE:-quay.io/keycloak/keycloak:latest}"
KEYCLOAK_REALM="${KEYCLOAK_REALM:-maas}"
KEYCLOAK_CLIENT_ID="${KEYCLOAK_CLIENT_ID:-maas-cli}"
KEYCLOAK_ADMIN_USER="${KEYCLOAK_ADMIN_USER:-admin}"
KEYCLOAK_ADMIN_PASSWORD="${KEYCLOAK_ADMIN_PASSWORD:-admin}"

if ! command -v oc >/dev/null 2>&1; then
  echo "error: oc is required" >&2
  exit 1
fi

CLUSTER_DOMAIN="${CLUSTER_DOMAIN:-$(oc get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')}"
if [[ -z "$CLUSTER_DOMAIN" ]]; then
  echo "error: could not determine cluster domain; set CLUSTER_DOMAIN" >&2
  exit 1
fi

KEYCLOAK_HOST="${KEYCLOAK_HOST:-keycloak.${CLUSTER_DOMAIN}}"
REALM_TEMPLATE="${DATA_DIR}/keycloak-maas-realm.json"
REALM_RENDERED="$(mktemp)"
cleanup() {
  rm -f "$REALM_RENDERED"
}
trap cleanup EXIT

sed \
  -e "s/__REALM__/${KEYCLOAK_REALM}/g" \
  -e "s/__CLIENT_ID__/${KEYCLOAK_CLIENT_ID}/g" \
  "$REALM_TEMPLATE" > "$REALM_RENDERED"

echo "[INFO] Installing temporary Keycloak in namespace ${KEYCLOAK_NAMESPACE}"
echo "[INFO] Route host: https://${KEYCLOAK_HOST}"

oc create namespace "${KEYCLOAK_NAMESPACE}" --dry-run=client -o yaml | oc apply -f -

oc create secret generic "${KEYCLOAK_NAME}-admin" \
  -n "${KEYCLOAK_NAMESPACE}" \
  --from-literal=username="${KEYCLOAK_ADMIN_USER}" \
  --from-literal=password="${KEYCLOAK_ADMIN_PASSWORD}" \
  --dry-run=client -o yaml | oc apply -f -

oc create configmap "${KEYCLOAK_NAME}-realm" \
  -n "${KEYCLOAK_NAMESPACE}" \
  --from-file=realm.json="${REALM_RENDERED}" \
  --dry-run=client -o yaml | oc apply -f -

cat <<EOF | oc apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${KEYCLOAK_NAME}
  namespace: ${KEYCLOAK_NAMESPACE}
  labels:
    app.kubernetes.io/name: ${KEYCLOAK_NAME}
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: ${KEYCLOAK_NAME}
  template:
    metadata:
      labels:
        app.kubernetes.io/name: ${KEYCLOAK_NAME}
    spec:
      containers:
      - name: keycloak
        image: ${KEYCLOAK_IMAGE}
        args:
        - start-dev
        - --import-realm
        env:
        - name: KC_BOOTSTRAP_ADMIN_USERNAME
          valueFrom:
            secretKeyRef:
              name: ${KEYCLOAK_NAME}-admin
              key: username
        - name: KC_BOOTSTRAP_ADMIN_PASSWORD
          valueFrom:
            secretKeyRef:
              name: ${KEYCLOAK_NAME}-admin
              key: password
        - name: KC_HTTP_ENABLED
          value: "true"
        - name: KC_PROXY_HEADERS
          value: xforwarded
        - name: KC_HOSTNAME
          value: ${KEYCLOAK_HOST}
        - name: KC_HOSTNAME_STRICT
          value: "false"
        ports:
        - containerPort: 8080
          name: http
        readinessProbe:
          httpGet:
            path: /realms/master
            port: http
          initialDelaySeconds: 20
          periodSeconds: 10
        livenessProbe:
          httpGet:
            path: /realms/master
            port: http
          initialDelaySeconds: 60
          periodSeconds: 20
        volumeMounts:
        - name: realm-import
          mountPath: /opt/keycloak/data/import
          readOnly: true
      volumes:
      - name: realm-import
        configMap:
          name: ${KEYCLOAK_NAME}-realm
---
apiVersion: v1
kind: Service
metadata:
  name: ${KEYCLOAK_NAME}
  namespace: ${KEYCLOAK_NAMESPACE}
spec:
  selector:
    app.kubernetes.io/name: ${KEYCLOAK_NAME}
  ports:
  - name: http
    port: 8080
    targetPort: http
---
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: ${KEYCLOAK_NAME}
  namespace: ${KEYCLOAK_NAMESPACE}
spec:
  host: ${KEYCLOAK_HOST}
  to:
    kind: Service
    name: ${KEYCLOAK_NAME}
  port:
    targetPort: http
  tls:
    termination: edge
    insecureEdgeTerminationPolicy: Redirect
EOF

oc rollout status deployment/"${KEYCLOAK_NAME}" -n "${KEYCLOAK_NAMESPACE}" --timeout=5m

ISSUER_URL="https://${KEYCLOAK_HOST}/realms/${KEYCLOAK_REALM}"
TOKEN_URL="https://${KEYCLOAK_HOST}/realms/${KEYCLOAK_REALM}/protocol/openid-connect/token"

cat <<EOF

[INFO] Keycloak is ready.
[INFO] Issuer URL: ${ISSUER_URL}
[INFO] Token endpoint: ${TOKEN_URL}
[INFO] Example users:
  - alice / letmein  -> premium-group
  - erin  / letmein  -> enterprise-group
  - ada   / letmein  -> admin-group

[INFO] Example token request:
curl -sk -X POST "${TOKEN_URL}" \\
  -d "grant_type=password" \\
  -d "client_id=${KEYCLOAK_CLIENT_ID}" \\
  -d "username=alice" \\
  -d "password=letmein" | jq -r '.access_token'

[INFO] Set deployment/overlays/odh/params.env:
oidc-issuer-url=${ISSUER_URL}
EOF
