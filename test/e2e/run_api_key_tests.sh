#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

echo "========================================"
echo "  API Key Management E2E Tests"
echo "========================================"

# Activate virtual environment
if [[ ! -d ".venv" ]]; then
    echo "[setup] Virtual environment not found. Run bootstrap.sh first."
    exit 1
fi

source .venv/bin/activate

# Auto-discover MAAS_API_BASE_URL if not set
if [[ -z "${MAAS_API_BASE_URL:-}" ]]; then
    echo "[setup] MAAS_API_BASE_URL not set, attempting to discover from OpenShift route..."
    ROUTE_HOST=$(oc get route maas-api -n maas-system -o jsonpath='{.spec.host}' 2>/dev/null || echo "")
    if [[ -n "$ROUTE_HOST" ]]; then
        export MAAS_API_BASE_URL="https://${ROUTE_HOST}/maas-api"
        echo "[setup] Discovered MAAS_API_BASE_URL: $MAAS_API_BASE_URL"
    else
        echo "[ERROR] Could not discover MAAS_API_BASE_URL. Please set it manually:"
        echo "  export MAAS_API_BASE_URL=https://your-maas-api-url/maas-api"
        exit 1
    fi
fi

# Auto-obtain token if not set
if [[ -z "${TOKEN:-}" ]]; then
    echo "[setup] TOKEN not set, obtaining via 'oc whoami -t'..."
    export TOKEN=$(oc whoami -t 2>/dev/null || echo "")
    if [[ -z "$TOKEN" ]]; then
        echo "[ERROR] Could not obtain token. Please set it manually:"
        echo "  export TOKEN=\$(oc whoami -t)"
        exit 1
    fi
    echo "[setup] Obtained TOKEN (length: ${#TOKEN})"
fi

# Optional admin token for authorization tests
if [[ -z "${ADMIN_OC_TOKEN:-}" ]]; then
    echo "[setup] ADMIN_OC_TOKEN not set. Admin authorization tests will be skipped."
    echo "[setup] To run admin tests, set: export ADMIN_OC_TOKEN=<admin-token>"
else
    echo "[setup] ADMIN_OC_TOKEN is set (length: ${#ADMIN_OC_TOKEN})"
fi

echo ""
echo "[run] Environment:"
echo "  MAAS_API_BASE_URL: $MAAS_API_BASE_URL"
echo "  TOKEN: ${TOKEN:0:10}... (${#TOKEN} chars)"
echo "  ADMIN_OC_TOKEN: ${ADMIN_OC_TOKEN:+SET}"
echo ""

# Create reports directory
mkdir -p reports

# Run the tests
echo "[run] Running API Key tests with pytest..."
pytest tests/test_api_keys.py -v \
    --html=reports/api-keys-report.html \
    --self-contained-html \
    -o log_cli=true \
    -o log_cli_level=INFO

echo ""
echo "========================================"
echo "  Tests Complete!"
echo "========================================"
echo "Report: $SCRIPT_DIR/reports/api-keys-report.html"
