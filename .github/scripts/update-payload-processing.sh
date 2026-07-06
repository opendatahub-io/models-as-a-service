#!/bin/bash
# Update pinned ai-gateway-payload-processing commit and image tag references.
#
# Usage: ./.github/scripts/update-payload-processing.sh <commit>
# Example: ./.github/scripts/update-payload-processing.sh 36614760abfa1b3fb2b521a89097bdaf6e0693b5

set -euo pipefail

if [ $# -lt 1 ]; then
    echo "Error: commit argument required"
    echo "Usage: $0 <commit>"
    exit 1
fi

COMMIT="$1"
if ! [[ "$COMMIT" =~ ^[0-9a-f]{40}$ ]]; then
    echo "Error: commit must be a 40-character lowercase hex SHA"
    exit 1
fi

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
IMAGE="quay.io/opendatahub/odh-ai-gateway-payload-processing:${COMMIT}"

echo "Updating payload-processing pin to ${COMMIT}"
echo "Image: ${IMAGE}"
echo ""

PARAMS_FILES=(
    "deployment/overlays/odh/params.env"
    "deployment/base/maas-controller/default/params.env"
)
for file in "${PARAMS_FILES[@]}"; do
    path="${PROJECT_ROOT}/${file}"
    if [ -f "$path" ]; then
        sed -i "s#^payload-processing-image=.*#payload-processing-image=${IMAGE}#" "$path"
        echo "  - ${file}"
    fi
done

KUSTOMIZATION="${PROJECT_ROOT}/deployment/base/payload-processing/manager/kustomization.yaml"
if [ -f "$KUSTOMIZATION" ]; then
    sed -i "s/\(newTag: \).*/\1${COMMIT}/" "$KUSTOMIZATION"
    echo "  - deployment/base/payload-processing/manager/kustomization.yaml"
fi

CONSTANTS="${PROJECT_ROOT}/maas-controller/pkg/platform/tenantreconcile/constants.go"
if [ -f "$CONSTANTS" ]; then
    sed -i "s#DefaultPayloadProcessingImage  = \".*\"#DefaultPayloadProcessingImage  = \"${IMAGE}\"#" "$CONSTANTS"
    echo "  - maas-controller/pkg/platform/tenantreconcile/constants.go"
fi

LOCAL_DEPLOY="${PROJECT_ROOT}/test/e2e/scripts/local-deploy.sh"
if [ -f "$LOCAL_DEPLOY" ]; then
    sed -i "s#IPP_IMAGE=\"\${IPP_IMAGE:-quay.io/opendatahub/odh-ai-gateway-payload-processing:.*}\"#IPP_IMAGE=\"\${IPP_IMAGE:-${IMAGE}}\"#" "$LOCAL_DEPLOY"
    echo "  - test/e2e/scripts/local-deploy.sh"
fi

echo ""
echo "Payload-processing pin update complete."
cd "$PROJECT_ROOT"
git diff --stat \
    deployment/overlays/odh/params.env \
    deployment/base/maas-controller/default/params.env \
    deployment/base/payload-processing/manager/kustomization.yaml \
    maas-controller/pkg/platform/tenantreconcile/constants.go \
    test/e2e/scripts/local-deploy.sh 2>/dev/null || true
