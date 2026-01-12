#!/bin/bash
# Script to update MAAS_REF references from "main" to a release tag
# Usage: ./update-maas-ref.sh <tag>
# Example: ./update-maas-ref.sh v1.0.0

set -euo pipefail

if [ $# -lt 1 ]; then
    echo "Error: Tag argument required"
    echo "Usage: $0 <tag>"
    echo "Example: $0 v1.0.0"
    exit 1
fi

TAG="$1"
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

echo "Updating MAAS_REF references from 'main' to '$TAG'"
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
    FILE_UPDATED=false
    
    # Check if file contains MAAS_REF references
    if grep -q "MAAS_REF" "$FILE_PATH"; then
        # Create a backup (for safety, though we're in a git repo)
        cp "$FILE_PATH" "${FILE_PATH}.bak"
        
        # Update various patterns:
        # 1. export MAAS_REF="main" (with or without comment)
        # 2. MAAS_REF="main"
        # 3. MAAS_REF:=main (shell default assignment)
        # 4. "${MAAS_REF:=main}" (in variable expansion)
        
        # Use sed with more specific patterns that only match MAAS_REF assignments
        # Patterns handle:
        # - export MAAS_REF="main" (with or without trailing comments)
        # - MAAS_REF="main"
        # - MAAS_REF:=main (shell default assignment)
        # - "${MAAS_REF:=main}" (in variable expansion)
        sed -i \
            -e "s|export MAAS_REF=\"main\"|export MAAS_REF=\"$TAG\"|g" \
            -e "s|export MAAS_REF='main'|export MAAS_REF='$TAG'|g" \
            -e "s|MAAS_REF=\"main\"|MAAS_REF=\"$TAG\"|g" \
            -e "s|MAAS_REF='main'|MAAS_REF='$TAG'|g" \
            -e "s|MAAS_REF:=main|MAAS_REF:=$TAG|g" \
            -e "s|\"\${MAAS_REF:=main}\"|\"\${MAAS_REF:=$TAG}\"|g" \
            "$FILE_PATH"
        
        # Check if file was actually modified
        if ! diff -q "${FILE_PATH}.bak" "$FILE_PATH" > /dev/null 2>&1; then
            FILE_UPDATED=true
            UPDATED_COUNT=$((UPDATED_COUNT + 1))
            echo "✅ Updated: $file"
        else
            echo "ℹ️  No changes needed: $file (MAAS_REF already set or not using 'main')"
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
    echo "ℹ️  No files required updates (MAAS_REF may already be set to tag or not present)"
fi

