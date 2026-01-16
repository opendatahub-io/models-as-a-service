#!/bin/bash
# Script to update MAAS_REF references from "main" or any semantic version to a release tag
# Usage: ./update-maas-ref.sh <tag>
# Example: ./update-maas-ref.sh v1.0.0
#
# This script will replace:
# - MAAS_REF="main" -> MAAS_REF="<tag>"
# - MAAS_REF="v1.0.3" -> MAAS_REF="<tag>"
# - MAAS_REF="1.0.3" -> MAAS_REF="<tag>"
# - Any other semantic version pattern

set -euo pipefail

if [ $# -lt 1 ]; then
    echo "Error: Tag argument required"
    echo "Usage: $0 <tag>"
    echo "Example: $0 v1.0.0"
    exit 1
fi

TAG="$1"
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

echo "Updating MAAS_REF references from 'main' or any semantic version to '$TAG'"
echo ""

# List of specific files to update (to avoid false positives)
FILES_TO_UPDATE=(
    "docs/content/quickstart.md"
    "scripts/deploy-rhoai-stable.sh"
)

UPDATED_COUNT=0
TOTAL_COUNT=0

for file in "${FILES_TO_UPDATE[@]}"; do
    FILE_PATH="${PROJECT_ROOT}/${file}"
    
    if [ ! -f "$FILE_PATH" ]; then
        echo "⚠️  Warning: $file not found, skipping"
        continue
    fi
    
    TOTAL_COUNT=$((TOTAL_COUNT + 1))
    
    # Check if file contains MAAS_REF references
    if grep -q "MAAS_REF" "$FILE_PATH"; then
        # Create a backup (for safety, though we're in a git repo)
        cp "$FILE_PATH" "${FILE_PATH}.bak"
        
        # Update various patterns to replace "main" OR any semantic version (v?[0-9]+\.[0-9]+\.[0-9]+)
        # This will match: main, v1.0.0, 1.0.0, v1.0.3, 1.0.3, etc.
        # Patterns handled:
        # 1. export MAAS_REF="main" or export MAAS_REF="v1.0.3" -> export MAAS_REF="<tag>"
        # 2. MAAS_REF="main" or MAAS_REF="v1.0.3" -> MAAS_REF="<tag>"
        # 3. MAAS_REF:=main or MAAS_REF:=v1.0.3 -> MAAS_REF:=<tag>
        # 4. "${MAAS_REF:=main}" or "${MAAS_REF:=v1.0.3}" -> "${MAAS_REF:=<tag>}"
        
        # Use sed with extended regex (-E) to match either "main" or semantic versions
        # Pattern: (main|v?[0-9]+\.[0-9]+\.[0-9]+) matches "main" or semantic version with optional v prefix
        # Using # as delimiter to avoid conflicts with | in the regex pattern
        sed -i -E \
            -e "s#export MAAS_REF=\"(main|v?[0-9]+\.[0-9]+\.[0-9]+)\"#export MAAS_REF=\"$TAG\"#g" \
            -e "s#export MAAS_REF='(main|v?[0-9]+\.[0-9]+\.[0-9]+)'#export MAAS_REF='$TAG'#g" \
            -e "s#MAAS_REF=\"(main|v?[0-9]+\.[0-9]+\.[0-9]+)\"#MAAS_REF=\"$TAG\"#g" \
            -e "s#MAAS_REF='(main|v?[0-9]+\.[0-9]+\.[0-9]+)'#MAAS_REF='$TAG'#g" \
            -e "s#MAAS_REF:=(main|v?[0-9]+\.[0-9]+\.[0-9]+)#MAAS_REF:=$TAG#g" \
            -e "s#\"\\\$\{MAAS_REF:=(main|v?[0-9]+\.[0-9]+\.[0-9]+)\}\"#\"\\\$\{MAAS_REF:=$TAG\}\"#g" \
            "$FILE_PATH"
        
        # Check if file was actually modified
        if ! diff -q "${FILE_PATH}.bak" "$FILE_PATH" > /dev/null 2>&1; then
            UPDATED_COUNT=$((UPDATED_COUNT + 1))
            echo "✅ Updated: $file"
        else
            echo "ℹ️  No changes needed: $file (MAAS_REF already set to '$TAG' or pattern not matched)"
        fi
        
        # Remove backup
        rm -f "${FILE_PATH}.bak"
    else
        echo "ℹ️  Skipped: $file (no MAAS_REF references found)"
    fi
done

echo ""
if [ $UPDATED_COUNT -gt 0 ]; then
    echo "✓ Successfully updated $UPDATED_COUNT of $TOTAL_COUNT files"
    echo ""
    echo "Changes made:"
    cd "$PROJECT_ROOT"
    git diff --stat || true
else
    echo "ℹ️  No files required updates (MAAS_REF may already be set to '$TAG' or not present)"
fi

