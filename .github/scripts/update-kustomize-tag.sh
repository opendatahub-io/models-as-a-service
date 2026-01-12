#!/bin/bash
# Script to update image tags in kustomization.yaml files
# Usage: ./.github/scripts/update-kustomize-tag.sh <tag>
# Example: ./.github/scripts/update-kustomize-tag.sh v1.0.0

set -euo pipefail

if [ $# -lt 1 ]; then
    echo "Error: Tag argument required"
    echo "Usage: $0 <tag>"
    echo "Example: $0 v1.0.0"
    exit 1
fi

TAG="$1"
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

echo "Updating kustomization.yaml files with tag: $TAG"

# Update base maas-api kustomization.yaml
BASE_KUSTOMIZATION="${PROJECT_ROOT}/deployment/base/maas-api/kustomization.yaml"
if [ -f "$BASE_KUSTOMIZATION" ]; then
    echo "  - Updating ${BASE_KUSTOMIZATION}"
    # Use sed to update the newTag field, preserving indentation
    sed -i "s/\(newTag: \).*/\1${TAG}/" "$BASE_KUSTOMIZATION"
fi

# Update dev overlay kustomization.yaml
DEV_KUSTOMIZATION="${PROJECT_ROOT}/maas-api/deploy/overlays/dev/kustomization.yaml"
if [ -f "$DEV_KUSTOMIZATION" ]; then
    echo "  - Updating ${DEV_KUSTOMIZATION}"
    sed -i "s/\(newTag: \).*/\1${TAG}/" "$DEV_KUSTOMIZATION"
fi

# Update ODH overlay params.env if it exists
ODH_PARAMS="${PROJECT_ROOT}/maas-api/deploy/overlays/odh/params.env"
if [ -f "$ODH_PARAMS" ]; then
    echo "  - Updating ${ODH_PARAMS}"
    # Update the image tag in params.env (format: quay.io/opendatahub/maas-api:tag)
    sed -i "s|\(maas-api-image=quay.io/opendatahub/maas-api:\).*|\1${TAG}|" "$ODH_PARAMS"
fi

echo "Tag update complete!"
echo ""
echo "Updated files:"
echo "  - ${BASE_KUSTOMIZATION}"
[ -f "$DEV_KUSTOMIZATION" ] && echo "  - ${DEV_KUSTOMIZATION}"
[ -f "$ODH_PARAMS" ] && echo "  - ${ODH_PARAMS}"

