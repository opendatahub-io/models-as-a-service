"""OIDCReady condition E2E tests — RHAI-31081.

Verifies the AITenant's OIDCReady status condition (opendatahub-io/models-as-a-service#1863)
end to end in the external-OIDC lane: the happy path, the common failure reasons,
recovery after a configuration fix, and removal when OIDC is unset. The condition
must never affect the tenant's overall health: Ready stays True and API-key access
keeps working while OIDC is broken.

Failure modes that need a controlled lying issuer (IssuerMismatch, JWKSUnavailable)
are covered by unit tests in maas-controller/pkg/oidcprobe, not here.

Prerequisites: EXTERNAL_OIDC=true (the default AITenant is patched with
spec.oidc by scripts/deploy.sh). Marked serial: every test mutates the shared
default AITenant, so the module must not run concurrently with modules that
depend on its OIDC config. In the nightly pass (single worker, alphabetical
collection) it runs after the consumer-portal and external-OIDC modules, and
the autouse fixture restores the deployed spec.oidc on teardown so later
modules are unaffected.
"""

from __future__ import annotations

import json
import os

import pytest
import requests

from conftest import TLS_VERIFY
from grid_oidc_helpers import required_grid_env
from multitenancy_helpers import (
    AITENANT_NAMESPACE,
    _oc_run,
    get_json_or_none,
    wait_for_json,
)
from test_helper import (
    TIMEOUT,
    _create_api_key,
    _get_cluster_token,
)

pytestmark = [
    pytest.mark.nightly,
    pytest.mark.external,
    pytest.mark.full_suite,
    # Mutates the shared default AITenant (cluster-wide state every OIDC module
    # depends on); runs in the serial pass per the PR #1802 convention.
    pytest.mark.serial,
    pytest.mark.skipif(
        os.environ.get("EXTERNAL_OIDC", "").lower() != "true",
        reason="EXTERNAL_OIDC is not true",
    ),
    pytest.mark.xdist_group("oidc_condition"),
]

DEFAULT_AITENANT_NAME = os.environ.get("DEFAULT_AITENANT_NAME", "models-as-a-service")

OIDC_READY_TYPE = "OIDCReady"

# Reconciles fire immediately on the generation change and the probe request
# itself is capped at 5s, so 180s leaves ample room for slow CI clusters.
CONDITION_TIMEOUT = 180

# The deployed spec.oidc, snapshotted by the autouse fixture. Every test
# replaces /spec/oidc wholesale (JSON patch) so mutations never leak between
# tests the way nested merge patches would.
_snapshot: dict = {"oidc": None}


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _patch_aitenant(patch: dict | list, *, patch_type: str = "json") -> None:
    result = _oc_run(
        [
            "patch", "aitenant", DEFAULT_AITENANT_NAME,
            "-n", AITENANT_NAMESPACE,
            "--type", patch_type,
            "-p", json.dumps(patch),
        ],
        timeout=60,
    )
    assert result.returncode == 0, (
        f"Failed to patch AITenant {DEFAULT_AITENANT_NAME}: "
        f"{result.stderr.strip() or result.stdout.strip()}"
    )


def _replace_oidc(oidc: dict | None) -> None:
    """Replace spec.oidc wholesale (None removes it)."""
    if oidc is None:
        _patch_aitenant([{"op": "remove", "path": "/spec/oidc"}])
    else:
        _patch_aitenant([{"op": "replace", "path": "/spec/oidc", "value": oidc}])


def _get_aitenant() -> dict:
    aitenant = get_json_or_none("aitenant", DEFAULT_AITENANT_NAME, AITENANT_NAMESPACE)
    assert aitenant is not None, (
        f"Default AITenant {DEFAULT_AITENANT_NAME} not found in {AITENANT_NAMESPACE}; "
        "the external-OIDC lane must deploy it (scripts/deploy.sh)"
    )
    return aitenant


def _find_condition(aitenant: dict, condition_type: str) -> dict | None:
    for condition in (aitenant.get("status") or {}).get("conditions") or []:
        if condition.get("type") == condition_type:
            return condition
    return None


def _wait_for_oidc_ready(
    expected_status: str,
    expected_reason: str | None = None,
    *,
    timeout: int = CONDITION_TIMEOUT,
) -> dict | None:
    """Wait for the OIDCReady condition to reach the expected state.

    Returns the condition (None when the wait is for the condition's absence).
    """
    expected_absent = expected_status == "absent"

    def _predicate(aitenant: dict) -> bool:
        condition = _find_condition(aitenant, OIDC_READY_TYPE)
        if expected_absent:
            return condition is None
        if condition is None or condition.get("status") != expected_status:
            return False
        return expected_reason is None or condition.get("reason") == expected_reason

    wait_for_json(
        "aitenant",
        DEFAULT_AITENANT_NAME,
        AITENANT_NAMESPACE,
        predicate=_predicate,
        timeout=timeout,
    )
    return _find_condition(_get_aitenant(), OIDC_READY_TYPE)


def _apply_secret(name: str, namespace: str, data: dict[str, str]) -> None:
    result = _oc_run(
        ["create", "secret", "generic", name, "-n", namespace]
        + [arg for key, value in data.items() for arg in ("--from-literal", f"{key}={value}")],
        timeout=60,
    )
    assert result.returncode == 0, (
        f"Failed to create Secret {namespace}/{name}: "
        f"{result.stderr.strip() or result.stdout.strip()}"
    )


def _delete_secret(name: str, namespace: str) -> None:
    result = _oc_run(["delete", "secret", name, "-n", namespace, "--ignore-not-found"], timeout=60)
    assert result.returncode == 0, (
        f"Failed to delete Secret {namespace}/{name}: "
        f"{result.stderr.strip() or result.stdout.strip()}"
    )


def _original_issuer_url() -> str:
    issuer = required_grid_env("OIDC_ISSUER_URL")
    assert issuer.startswith("https://"), f"OIDC_ISSUER_URL must be https, got {issuer!r}"
    return issuer


def _oidc_with(overrides: dict) -> dict:
    """The deployed OIDC config with field overrides applied."""
    assert _snapshot["oidc"] is not None, "fixture must snapshot spec.oidc first"
    oidc = dict(_snapshot["oidc"])
    oidc.update(overrides)
    return oidc


@pytest.fixture(scope="module", autouse=True)
def restore_default_aitenant_oidc():
    """Snapshot spec.oidc and restore it after the module.

    Every test in this module mutates the default AITenant's OIDC config; the
    restore keeps later modules (and re-runs) independent of this one. The
    final wait also proves the tenant converges back to its deployed state.
    """
    _snapshot["oidc"] = (_get_aitenant().get("spec") or {}).get("oidc")
    assert _snapshot["oidc"] is not None, (
        "The default AITenant must have spec.oidc set when EXTERNAL_OIDC=true"
    )
    yield
    _replace_oidc(_snapshot["oidc"])
    _wait_for_oidc_ready("True", "Available")


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

class TestOIDCReadyCondition:
    def test_oidc_ready_available(self):
        """Healthy issuer: True/Available with the observed generation, and the
        tenant's overall health (Ready, phase) is reported alongside it."""
        aitenant = _get_aitenant()
        condition = _wait_for_oidc_ready("True", "Available")

        assert condition is not None
        assert condition.get("observedGeneration") == aitenant["metadata"].get("generation"), (
            f"OIDCReady must track the AITenant generation, got {condition}"
        )
        assert _original_issuer_url().rstrip("/") in (condition.get("message") or ""), (
            f"OIDCReady message should name the issuer, got {condition}"
        )

        ready = _find_condition(aitenant, "Ready")
        assert ready is not None and ready.get("status") == "True", (
            f"Ready must be True alongside a healthy OIDCReady, got {ready}"
        )
        assert (aitenant.get("status") or {}).get("phase") == "Active"

    def test_oidc_ready_issuer_unreachable(self):
        """Unresolvable issuer host: False/IssuerUnreachable."""
        _replace_oidc(_oidc_with({"issuerUrl": "https://oidc-unreachable.invalid/realms/tenant-a"}))
        condition = _wait_for_oidc_ready("False", "IssuerUnreachable")

        assert condition is not None
        assert (condition.get("message") or "").strip(), (
            f"IssuerUnreachable must carry an actionable message, got {condition}"
        )

    def test_oidc_ready_discovery_failed_and_api_keys_unaffected(self, maas_api_base_url: str):
        """Reachable host, missing realm (404 discovery): False/DiscoveryFailed,
        while the tenant stays Ready and API-key access keeps working — an OIDC
        outage must not degrade API traffic."""
        missing_realm_issuer = _original_issuer_url().rstrip("/") + "-no-such-realm"
        _replace_oidc(_oidc_with({"issuerUrl": missing_realm_issuer}))
        condition = _wait_for_oidc_ready("False", "DiscoveryFailed")
        assert condition is not None

        aitenant = _get_aitenant()
        ready = _find_condition(aitenant, "Ready")
        assert ready is not None and ready.get("status") == "True", (
            "A broken OIDC issuer must not change the Ready condition"
        )
        assert (aitenant.get("status") or {}).get("phase") == "Active"

        api_key = _create_api_key(_get_cluster_token(), name=f"e2e-oidc-ready-{os.getpid()}")
        response = requests.get(
            f"{maas_api_base_url}/v1/models",
            headers={"Authorization": f"Bearer {api_key}"},
            timeout=TIMEOUT,
            verify=TLS_VERIFY,
        )
        assert response.status_code == 200, (
            f"API-key access must keep working during an OIDC outage, got "
            f"{response.status_code} {response.text[:200]}"
        )

    def test_oidc_ready_client_config_incomplete(self):
        """clientSecretRef pointing at a Secret that exists but lacks the
        expected key: False/ClientConfigIncomplete, naming the missing key.

        The Secret is created in the tenant namespace (where the controller
        resolves clientSecretRef) with a different key, so the missing-key
        branch is exercised rather than the secret-not-found one.
        """
        tenant_namespace = (_get_aitenant().get("status") or {}).get("tenantNamespace")
        assert tenant_namespace, "AITenant status.tenantNamespace must be populated"
        secret_name = f"e2e-portal-missing-key-{os.getpid()}"
        _apply_secret(secret_name, tenant_namespace, {"wrong-key": "value"})
        try:
            _replace_oidc(_oidc_with({
                "clientSecretRef": {"name": secret_name, "key": "client-secret"},
            }))
            condition = _wait_for_oidc_ready("False", "ClientConfigIncomplete")
        finally:
            _delete_secret(secret_name, tenant_namespace)

        assert condition is not None
        assert "client-secret" in (condition.get("message") or ""), (
            f"ClientConfigIncomplete should name the missing key, got {condition}"
        )

    def test_oidc_ready_trailing_slash_tolerated(self):
        """A trailing slash on issuerUrl still matches the discovery issuer."""
        _replace_oidc(_oidc_with({"issuerUrl": _original_issuer_url().rstrip("/") + "/"}))
        _wait_for_oidc_ready("True", "Available")

    def test_oidc_ready_recovers_after_fix(self):
        """A broken issuer that is fixed again returns to Available on the next
        reconcile, without recreating the tenant."""
        _replace_oidc(_oidc_with({"issuerUrl": _original_issuer_url().rstrip("/") + "-no-such-realm"}))
        _wait_for_oidc_ready("False", "DiscoveryFailed")

        _replace_oidc(_oidc_with({"issuerUrl": _original_issuer_url()}))
        condition = _wait_for_oidc_ready("True", "Available")
        assert condition is not None

    def test_oidc_ready_removed_when_oidc_unset(self):
        """Removing spec.oidc removes the condition entirely (not False)."""
        _replace_oidc(None)
        _wait_for_oidc_ready("absent")
