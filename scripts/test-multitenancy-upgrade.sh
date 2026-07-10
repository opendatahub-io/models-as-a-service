#!/usr/bin/env bash
#
# Multitenancy upgrade test helper (ODH 3.4 -> 3.5 and RHOAI 3.4 -> 3.5).
#
# Validates the no-touch migration described in:
#   temp/multitenancy/ODH-ADR-MS-0003-ai-gateway-tenancy-v6.md
#
# Usage:
#   ./scripts/test-multitenancy-upgrade.sh install-odh-34
#   ./scripts/test-multitenancy-upgrade.sh validate-pre
#   ./scripts/test-multitenancy-upgrade.sh upgrade-odh-35
#   ./scripts/test-multitenancy-upgrade.sh validate-post
#   ./scripts/test-multitenancy-upgrade.sh run-e2e
#   ./scripts/test-multitenancy-upgrade.sh install-rhoai-34
#   ./scripts/test-multitenancy-upgrade.sh upgrade-rhoai-35
#
# Environment overrides:
#   MAAS_CONTROLLER_IMAGE / MAAS_API_IMAGE  - optional custom MaaS images (main branch builds)
#   OPERATOR_CATALOG                        - custom operator catalog for pre-release builds
#   SKIP_DEPLOY=1                           - skip deploy.sh during install-* (cluster already has 3.4)
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

ODH_34_CSV="${ODH_34_CSV:-opendatahub-operator.v3.4.0}"
ODH_35_CSV="${ODH_35_CSV:-opendatahub-operator.v3.5.0-ea.2}"
RHOAI_34_CSV="${RHOAI_34_CSV:-rhods-operator.3.4.2}"
RHOAI_35_CSV="${RHOAI_35_CSV:-rhods-operator.3.5.0-ea.1}"

ODH_OPERATOR_NS="${ODH_OPERATOR_NS:-opendatahub}"
RHOAI_OPERATOR_NS="${RHOAI_OPERATOR_NS:-redhat-ods-operator}"
AITENANT_NS="${AITENANT_NAMESPACE:-ai-tenants}"
DEFAULT_AITENANT="${DEFAULT_AITENANT_NAME:-models-as-a-service}"

log() { echo "[$(date -u +%H:%M:%S)] $*"; }
die() { echo "ERROR: $*" >&2; exit 1; }

require_oc() {
  oc whoami >/dev/null 2>&1 || die "Not logged in. Run: oc login ..."
}

approve_installplan() {
  local ns=$1
  local timeout=${2:-300}
  local deadline=$((SECONDS + timeout))
  while [[ $SECONDS -lt $deadline ]]; do
    local ip
    ip=$(oc get installplan -n "$ns" -o json 2>/dev/null \
      | jq -r '.items[] | select((.spec.approved // false) == false) | .metadata.name' \
      | head -1)
    if [[ -n "$ip" ]]; then
      log "Approving InstallPlan $ip in $ns"
      oc patch installplan "$ip" -n "$ns" --type=merge -p '{"spec":{"approved":true}}' || true
      return 0
    fi
    sleep 5
  done
  log "No pending InstallPlan in $ns (may already be approved)"
}

wait_csv() {
  local ns=$1
  local csv=$2
  local timeout=${3:-600}
  log "Waiting for CSV $csv in $ns ..."
  oc wait --for=jsonpath='{.status.phase}'=Succeeded "csv/$csv" -n "$ns" --timeout="${timeout}s"
}

patch_subscription_csv() {
  local sub=$1
  local ns=$2
  local csv=$3
  log "Patching subscription $sub in $ns -> startingCSV=$csv"
  oc patch subscription "$sub" -n "$ns" --type=merge -p "{\"spec\":{\"startingCSV\":\"$csv\",\"installPlanApproval\":\"Manual\"}}"
  approve_installplan "$ns"
}

install_odh_34() {
  require_oc
  log "=== Phase 1: Install ODH $ODH_34_CSV ==="
  if [[ "${SKIP_DEPLOY:-}" != "1" ]]; then
    OPERATOR_STARTING_CSV="$ODH_34_CSV" \
    OPERATOR_CHANNEL=fast-3 \
    OPERATOR_INSTALL_PLAN_APPROVAL=Manual \
      "$REPO_ROOT/scripts/deploy.sh" --operator-type odh --deployment-mode operator
  else
    log "SKIP_DEPLOY=1 — assuming ODH 3.4 is already installed"
  fi
  validate_pre
}

install_rhoai_34() {
  require_oc
  log "=== Phase 1: Install RHOAI $RHOAI_34_CSV ==="
  if [[ "${SKIP_DEPLOY:-}" != "1" ]]; then
    OPERATOR_CHANNEL=stable-3.4 \
      "$REPO_ROOT/scripts/deploy.sh" --operator-type rhoai --deployment-mode operator
    patch_subscription_csv rhods-operator "$RHOAI_OPERATOR_NS" "$RHOAI_34_CSV"
    wait_csv "$RHOAI_OPERATOR_NS" "$RHOAI_34_CSV" 900
  else
    log "SKIP_DEPLOY=1 — assuming RHOAI 3.4 is already installed"
  fi
  validate_pre
}

validate_pre() {
  require_oc
  log "=== Pre-upgrade validation (3.4 baseline) ==="

  echo "--- Operator ---"
  oc get csv -n "$ODH_OPERATOR_NS" 2>/dev/null | grep -E 'opendatahub|NAME' || true
  oc get csv -n "$RHOAI_OPERATOR_NS" 2>/dev/null | grep -E 'rhods|NAME' || true

  echo "--- DSC ---"
  oc get datasciencecluster,dscinitialization -A

  echo "--- MaaS controller ---"
  oc get deploy -A | grep maas-controller || echo "(no maas-controller yet)"

  echo "--- Default tenant / AITenant (may not exist on pure 3.4) ---"
  oc get aitenant -n "$AITENANT_NS" 2>/dev/null || echo "(AITenant CRD or resources not present yet — expected on 3.4)"

  echo "--- Existing API keys snapshot ---"
  oc get secret -A 2>/dev/null | grep -i maas-db || true

  log "Pre-upgrade snapshot complete. Seed models/subscriptions/API keys now if you want migration coverage."
}

upgrade_odh_35() {
  require_oc
  log "=== Phase 2: Upgrade ODH -> $ODH_35_CSV ==="
  patch_subscription_csv opendatahub-operator "$ODH_OPERATOR_NS" "$ODH_35_CSV"
  wait_csv "$ODH_OPERATOR_NS" "$ODH_35_CSV" 900
  log "Waiting for DataScienceCluster after upgrade ..."
  # shellcheck source=deployment-helpers.sh
  source "$REPO_ROOT/scripts/deployment-helpers.sh"
  wait_datasciencecluster_ready default-dsc 900 || die "DSC not ready after upgrade"
  validate_post
}

upgrade_rhoai_35() {
  require_oc
  log "=== Phase 2: Upgrade RHOAI -> $RHOAI_35_CSV (beta channel) ==="
  oc patch subscription rhods-operator -n "$RHOAI_OPERATOR_NS" --type=merge \
    -p '{"spec":{"channel":"beta","installPlanApproval":"Manual"}}' || true
  patch_subscription_csv rhods-operator "$RHOAI_OPERATOR_NS" "$RHOAI_35_CSV"
  wait_csv "$RHOAI_OPERATOR_NS" "$RHOAI_35_CSV" 900
  source "$REPO_ROOT/scripts/deployment-helpers.sh"
  wait_datasciencecluster_ready default-dsc 900 || die "DSC not ready after upgrade"
  validate_post
}

validate_post() {
  require_oc
  log "=== Post-upgrade validation (ADR Phase 1-4) ==="

  local failures=0
  check() {
    local desc=$1
    shift
    if "$@"; then
      echo "  OK  $desc"
    else
      echo "  FAIL $desc"
      failures=$((failures + 1))
    fi
  }

  check "DSC Ready" oc get datasciencecluster default-dsc -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null | grep -q True
  check "AITenant CRD exists" oc get crd aitenants.maas.opendatahub.io >/dev/null 2>&1
  check "ai-tenants namespace exists" oc get ns "$AITENANT_NS" >/dev/null 2>&1
  check "Default AITenant exists" oc get aitenant "$DEFAULT_AITENANT" -n "$AITENANT_NS" >/dev/null 2>&1

  if oc get aitenant "$DEFAULT_AITENANT" -n "$AITENANT_NS" >/dev/null 2>&1; then
    local ready
    ready=$(oc get aitenant "$DEFAULT_AITENANT" -n "$AITENANT_NS" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")
    check "Default AITenant Ready=$ready" test "$ready" = "True"
  fi

  check "MaasTenantConfig in models-as-a-service" oc get maastenantconfig default-tenant -n models-as-a-service >/dev/null 2>&1
  check "maas-api in infra namespace" oc get deploy -A -o name 2>/dev/null | grep -q 'deployment.apps/maas-api'
  check "maas-controller running" bash -c 'oc get deploy maas-controller -n opendatahub >/dev/null 2>&1 || oc get deploy maas-controller -n redhat-ods-applications >/dev/null 2>&1'
  check "Gateway programmed" oc get gateway maas-default-gateway -n openshift-ingress -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null | grep -q True

  echo ""
  echo "--- Resource summary ---"
  oc get aitenant -n "$AITENANT_NS" 2>/dev/null || true
  oc get maastenantconfig -A 2>/dev/null || true
  oc get deploy -A 2>/dev/null | grep maas || true
  oc get maasmodelref,maassubscription,maasauthpolicy -A 2>/dev/null | head -20 || true

  if [[ $failures -gt 0 ]]; then
    die "$failures post-upgrade check(s) failed"
  fi
  log "Post-upgrade validation passed"
}

run_e2e() {
  require_oc
  log "=== Running multitenancy E2E tests ==="
  SKIP_DEPLOYMENT=true \
  SKIP_VALIDATION=false \
  ENABLE_TENANT_NAMESPACE_DISCOVERY=true \
    "$REPO_ROOT/test/e2e/scripts/prow_run_smoke_test.sh"
}

usage() {
  cat <<EOF
Multitenancy upgrade test helper

Commands:
  install-odh-34     Deploy ODH 3.4 via deploy.sh (pinned CSV)
  upgrade-odh-35     Upgrade ODH subscription to 3.5 and validate
  install-rhoai-34   Deploy RHOAI 3.4.2 and validate
  upgrade-rhoai-35   Upgrade RHOAI to 3.5 EA (beta channel) and validate
  validate-pre       Snapshot pre-upgrade state
  validate-post      ADR Phase 1-4 checks after upgrade
  run-e2e            Run multitenancy-focused pytest suite

Full ODH flow:
  ./scripts/test-multitenancy-upgrade.sh install-odh-34
  # optional: create models, API keys, subscriptions
  ./scripts/test-multitenancy-upgrade.sh upgrade-odh-35
  ./scripts/test-multitenancy-upgrade.sh run-e2e

Full RHOAI flow:
  ./scripts/test-multitenancy-upgrade.sh install-rhoai-34
  ./scripts/test-multitenancy-upgrade.sh upgrade-rhoai-35
  ./scripts/test-multitenancy-upgrade.sh run-e2e

Custom MaaS images from main:
  MAAS_CONTROLLER_IMAGE=quay.io/maas/maas-controller:<tag> \\
  MAAS_API_IMAGE=quay.io/maas/maas-api:<tag> \\
    ./scripts/test-multitenancy-upgrade.sh install-odh-34
EOF
}

main() {
  local cmd=${1:-}
  case "$cmd" in
    install-odh-34) install_odh_34 ;;
    upgrade-odh-35) upgrade_odh_35 ;;
    install-rhoai-34) install_rhoai_34 ;;
    upgrade-rhoai-35) upgrade_rhoai_35 ;;
    validate-pre) validate_pre ;;
    validate-post) validate_post ;;
    run-e2e) run_e2e ;;
    -h|--help|help|"") usage ;;
    *) die "Unknown command: $cmd (try --help)" ;;
  esac
}

main "$@"
