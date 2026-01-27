
#!/usr/bin/env bash
# ==========================================================
# Minimal OpenShift HTPasswd setup
# ==========================================================
# Creates two users:
#   - admin-user (cluster-admin)
#   - dev-user   (edit cluster-wide)
# Does NOT remove or modify existing identity providers/users
# Assumes 'htpasswd' is already installed.
# ==========================================================

set -euo pipefail

# --- Configuration ---
HTPASSWD_SECRET_NAME="htpasswd-secret"
IDP_NAME="htpasswd-provider"

# Openshift users
OPENSHIFT_ADMIN_USER="admin-user"
OPENSHIFT_DEV_USER="dev-user"

# Passwords
OPENSHIFT_ADMIN_PASS="${ADMIN_PASSWORD:-$(openssl rand -hex 12)}"
OPENSHIFT_DEV_PASS="${DEV_PASSWORD:-$(openssl rand -hex 12)}"

echo "=== Setting up HTPasswd Identity Provider ==="

# --- Create htpasswd file ---
{
  printf '%s\n' "$OPENSHIFT_ADMIN_PASS" | htpasswd -nB -C 12 -i "$OPENSHIFT_ADMIN_USER"
  printf '%s\n' "$OPENSHIFT_DEV_PASS"   | htpasswd -nB -C 12 -i "$OPENSHIFT_DEV_USER"
} | oc create secret generic "$HTPASSWD_SECRET_NAME" \
      --from-file=htpasswd=/dev/stdin \
      -n openshift-config --dry-run=client -o yaml | oc apply -f -

# --- Create or patch OAuth configuration ---
if ! oc get oauth cluster &>/dev/null; then
  echo "No existing OAuth found. Creating a new one..."
  cat <<EOF | oc apply -f -
apiVersion: config.openshift.io/v1
kind: OAuth
metadata:
  name: cluster
spec:
  identityProviders:
  - name: ${IDP_NAME}
    mappingMethod: claim
    type: HTPasswd
    htpasswd:
      fileData:
        name: ${HTPASSWD_SECRET_NAME}
EOF
else
  echo "OAuth exists. Checking if HTPasswd provider already exists..."
  
  # Check if our identity provider already exists
  if oc get oauth cluster -o jsonpath="{.spec.identityProviders[?(@.name=='${IDP_NAME}')].name}" 2>/dev/null | grep -q "${IDP_NAME}"; then
    echo "HTPasswd provider already exists, skipping..."
  else
    echo "Adding new HTPasswd provider..."
    # Create a simple patch file to avoid YAML escaping issues
    PATCH_FILE="/tmp/oauth-patch.json"
    # Cleanup
    trap 'rm -f "${PATCH_FILE}"' EXIT
    cat > "${PATCH_FILE}" <<JSONEOF
{
  "spec": {
    "identityProviders": [
      {
        "name": "${IDP_NAME}",
        "mappingMethod": "claim",
        "type": "HTPasswd",
        "htpasswd": {
          "fileData": {
            "name": "${HTPASSWD_SECRET_NAME}"
          }
        }
      }
    ]
  }
}
JSONEOF
    
    # Apply the patch from file
    if oc patch oauth cluster --type=merge --patch-file "${PATCH_FILE}"; then
      echo "Successfully added HTPasswd provider"
    else
      echo "Patch failed, but continuing..."
    fi
  fi
fi

# --- Wait for rollout ---
echo "Waiting for authentication rollout..."
echo "This may take several minutes as OAuth pods restart..."

# Manually trigger rollout restart to ensure OAuth picks up changes
oc -n openshift-authentication rollout restart deployment/oauth-openshift
oc -n openshift-authentication rollout status deployment/oauth-openshift --timeout=300s
echo "✅ OAuth deployment restarted and ready"

# --- Wait for identity provider to be fully ready ---
echo "Waiting for identity provider to be ready..."
sleep 10
echo "Checking OAuth configuration..."
if oc get oauth cluster -o jsonpath='{.spec.identityProviders[?(@.name=="'"${IDP_NAME}"'")]}' 2>/dev/null; then
    echo "✅ Identity provider configured successfully"
else
    echo "⚠️  WARNING: Could not verify identity provider configuration"
fi

# --- Grant roles ---
echo "Granting cluster-admin role to ${OPENSHIFT_ADMIN_USER}..."
oc adm policy add-cluster-role-to-user cluster-admin "${OPENSHIFT_ADMIN_USER}" 2>/dev/null || echo "Note: User will be created on first login"

echo "Granting edit role (cluster-wide) to ${OPENSHIFT_DEV_USER}..."
oc adm policy add-cluster-role-to-user edit "${OPENSHIFT_DEV_USER}" 2>/dev/null || echo "Note: User will be created on first login"

echo "=== Done! ==="
echo
echo "Users created:"
echo "  Admin user: ${OPENSHIFT_ADMIN_USER}"
echo "  Dev user:   ${OPENSHIFT_DEV_USER}"

cat <<EOF
export OPENSHIFT_ADMIN_USER="${OPENSHIFT_ADMIN_USER}"
export OPENSHIFT_ADMIN_PASS="${OPENSHIFT_ADMIN_PASS}"
export OPENSHIFT_DEV_USER="${OPENSHIFT_DEV_USER}"
export OPENSHIFT_DEV_PASS="${OPENSHIFT_DEV_PASS}"
EOF
echo "✅ Users setup completed"