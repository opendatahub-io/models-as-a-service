"""MaasTenantConfig apiKeys settings on a non-default tenant (RHOAIENG-100759).

Test that spec.apiKeys policy is enforced at a tenant's own maas-api without
leaking to sibling tenants.
"""

from __future__ import annotations

import contextlib
import json
import uuid
from collections.abc import Iterator
from typing import Any

import pytest
from multitenancy_helpers import (
    INFRA_NAMESPACE,
    TENANT_CONFIG_KIND,
    TENANT_CR_NAME,
    _oc_run,
    create_api_key_at,
    deployment_env,
    make_tenant_model_accessible,
    per_tenant_maas_api_names,
    provision_tenant_model,
    wait_for_json,
)
from test_helper import (
    _delete_cr,
    _delete_governance_and_wait,
    _get_cluster_token,
)

pytestmark = [
    # Two provisioned tenants and a full maas-api rollout each — too heavy for the
    # curated smoke path, but core enough that nightly-only would under-cover it.
    pytest.mark.full_suite,
    pytest.mark.tenant_isolation,
    pytest.mark.xdist_group("tenant_isolation"),
]

CONVERGENCE_TIMEOUT = 300

# Rendered onto every tenant's maas-api Deployment; read once at pod startup.
API_KEY_MAX_EXPIRATION_ENV = "API_KEY_MAX_EXPIRATION_DAYS"

# Far below the platform default so a key the tenant must reject stays acceptable
# elsewhere, which is what makes the namespace-scoping check meaningful.
TENANT_MAX_EXPIRATION_DAYS = 7
EXCEEDS_TENANT_WITHIN_DEFAULT_DAYS = 30
HOURS_PER_DAY = 24


def _non_default_tenant(shared_test_tenants) -> dict[str, str]:
    """Return the tenant to patch (tenant B), leaving tenant A as the untouched sibling."""
    return shared_test_tenants[1]


@contextlib.contextmanager
def _tenant_config_patched(
    tenant_namespace: str,
    spec_patch: dict[str, Any],
    *remove_paths: str,
    settle_deployment: str,
    settle_env: str,
    settle_namespace: str = INFRA_NAMESPACE,
) -> Iterator[None]:
    """Merge-patch a tenant's MaasTenantConfig and undo it on exit.

    Removing the patch starts a second rollout. The modules sharing this xdist
    group reuse the same tenants, so returning while maas-api is still rolling
    hands the next one a half-updated Deployment. The value to settle back to is
    read before patching rather than hardcoded, so it tracks whatever the
    renderer's default is.
    """
    original = deployment_env(settle_deployment, settle_namespace).get(settle_env)

    body = json.dumps({"spec": spec_patch})
    result = _oc_run(
        ["patch", TENANT_CONFIG_KIND, TENANT_CR_NAME, "-n", tenant_namespace, "--type=merge", "-p", body]
    )
    assert result.returncode == 0, (
        f"Failed to patch {TENANT_CONFIG_KIND}/{TENANT_CR_NAME} in '{tenant_namespace}': "
        f"{(result.stderr or result.stdout or '').strip()}"
    )
    body_failed = False
    try:
        yield
    except BaseException:
        # Teardown problems must not replace the failure the test actually hit.
        body_failed = True
        raise
    finally:
        removals = json.dumps([{"op": "remove", "path": path} for path in remove_paths])
        result = _oc_run(
            ["patch", TENANT_CONFIG_KIND, TENANT_CR_NAME, "-n", tenant_namespace, "--type=json", "-p", removals]
        )
        if result.returncode != 0:
            if not body_failed:
                raise AssertionError(
                    f"Failed to cleanup {TENANT_CONFIG_KIND}/{TENANT_CR_NAME} in '{tenant_namespace}': "
                    f"{(result.stderr or result.stdout or '').strip()}"
                )
        elif original is not None:
            try:
                _wait_for_env_rollout(settle_deployment, settle_namespace, settle_env, original)
            except BaseException:
                if not body_failed:
                    raise


def _wait_for_env_rollout(
    deployment_name: str, namespace: str, env_var: str, expected_value: str, timeout: int = CONVERGENCE_TIMEOUT
) -> None:
    # maas-api reads the cap once at startup, so the patched value only takes effect
    # when the new pod is the only one serving. Checking spec.env alone passes while
    # the old pod still answers, which reads as "policy not enforced".

    def _rolled_out(obj: dict[str, Any]) -> bool:
        spec = obj.get("spec") or {}
        status = obj.get("status") or {}
        metadata = obj.get("metadata") or {}

        containers = ((spec.get("template") or {}).get("spec") or {}).get("containers") or []
        if not containers:
            return False
        env = containers[0].get("env") or []
        if not any(v.get("name") == env_var and v.get("value") == expected_value for v in env):
            return False

        desired = spec.get("replicas")
        return (
            status.get("observedGeneration", 0) >= metadata.get("generation", 0)
            and status.get("replicas") == desired
            and status.get("updatedReplicas") == desired
            and status.get("readyReplicas") == desired
            and status.get("availableReplicas") == desired
        )

    wait_for_json("deployment", deployment_name, namespace, predicate=_rolled_out, timeout=timeout)


@pytest.fixture(scope="module")
def tenant_key_governance(shared_test_tenants):
    """Give both shared tenants a model, MaaSAuthPolicy and MaaSSubscription.

    Minting an API key through a tenant's own maas-api needs that tenant's gateway
    AuthPolicy programmed, which maas-controller only reconciles once a tenant
    MaaSAuthPolicy exists; without it every mint returns 500 AUTH_FAILURE. Mirrors
    test_tenant_auth_isolation.py, which is the proven path for tenant-scoped keys.
    """
    suffix = uuid.uuid4().hex[:6]
    policy_name = f"e2e-mt-tenantcfg-{suffix}"
    subscription_name = f"e2e-mt-tenantcfg-{suffix}"
    cases = [{**case, "model_name": f"tenantcfg-model-{case['suffix']}"} for case in shared_test_tenants]
    try:
        for case in cases:
            provision_tenant_model(case["model_name"], case["tenant_ns"], case["gateway_name"])
            make_tenant_model_accessible(
                case["model_name"],
                case["tenant_ns"],
                policy_name,
                subscription_name,
                gateway_name=case["gateway_name"],
                require_trlp_ready=False,
            )
        yield subscription_name
    finally:
        _delete_governance_and_wait(
            subscriptions=[(subscription_name, case["tenant_ns"]) for case in cases],
            auth_policies=[(policy_name, case["tenant_ns"]) for case in cases],
        )
        for case in cases:
            _delete_cr("maasmodelref", case["model_name"], case["tenant_ns"])
            _delete_cr("llminferenceservice", case["model_name"], case["tenant_ns"])


class TestNonDefaultTenantAPIKeysPolicy:
    """spec.apiKeys policy is enforced by the tenant's own maas-api and stays tenant-scoped."""

    def test_key_exceeding_tenant_policy_is_rejected(self, shared_test_tenants, tenant_key_governance):
        """Given spec.apiKeys.maxExpirationDays on a non-default MaasTenantConfig,
        when a key is minted through that tenant's maas-api with a longer expiry,
        then the mint is rejected with an error message citing the cap.
        """
        tenant = _non_default_tenant(shared_test_tenants)
        token = _get_cluster_token()
        deployment = per_tenant_maas_api_names(tenant["name"])["deployment"]
        with _tenant_config_patched(
            tenant["namespace"],
            {"apiKeys": {"maxExpirationDays": TENANT_MAX_EXPIRATION_DAYS}},
            "/spec/apiKeys",
            settle_deployment=deployment,
            settle_env=API_KEY_MAX_EXPIRATION_ENV,
        ):
            _wait_for_env_rollout(
                deployment,
                INFRA_NAMESPACE,
                API_KEY_MAX_EXPIRATION_ENV,
                str(TENANT_MAX_EXPIRATION_DAYS),
            )
            # Rollout complete: only the new pod serves, so the cap is live
            # Positive control: a key under the cap should succeed
            under_cap_response = create_api_key_at(
                tenant["base_url"],
                token,
                f"e2e-mt-policy-under-{uuid.uuid4().hex[:6]}",
                subscription=tenant_key_governance,
                expires_in=f"{TENANT_MAX_EXPIRATION_DAYS - 1}d",
            )
            assert under_cap_response.status_code in (200, 201), (
                f"Key under the {TENANT_MAX_EXPIRATION_DAYS}-day cap should succeed; "
                f"minting a {TENANT_MAX_EXPIRATION_DAYS - 1}-day key returned {under_cap_response.status_code}"
            )

            # Negative: exceeding the cap should be rejected with reason in response body
            over_cap_response = create_api_key_at(
                tenant["base_url"],
                token,
                f"e2e-mt-policy-exceeds-{uuid.uuid4().hex[:6]}",
                subscription=tenant_key_governance,
                expires_in=f"{EXCEEDS_TENANT_WITHIN_DEFAULT_DAYS * HOURS_PER_DAY}h",
            )
            assert over_cap_response.status_code == 400, (
                f"Minting a "
                f"{EXCEEDS_TENANT_WITHIN_DEFAULT_DAYS}-day key returned {over_cap_response.status_code} "
                f"instead of 400 — the cap is known to the service but not enforced on create"
            )
            # Must be the cap that rejected it, not some other 400. The day count
            # proves the tenant's own value was applied rather than the platform default.
            error = over_cap_response.json().get("error", "")
            assert "exceeds maximum allowed" in error, (
                f"Rejection should cite the expiration cap; got: {error!r}"
            )
            assert f"({TENANT_MAX_EXPIRATION_DAYS} days)" in error, (
                f"Rejection should cite this tenant's {TENANT_MAX_EXPIRATION_DAYS}-day cap rather than "
                f"the platform default; got: {error!r}"
            )

    def test_policy_does_not_apply_to_sibling_tenant(self, shared_test_tenants, tenant_key_governance):
        """Given spec.apiKeys.maxExpirationDays set on one non-default tenant,
        when the same over-limit key is minted through a sibling tenant's maas-api,
        then it succeeds, showing the policy is namespace-scoped.
        """
        tenant = _non_default_tenant(shared_test_tenants)
        sibling = shared_test_tenants[0]
        token = _get_cluster_token()
        deployment = per_tenant_maas_api_names(tenant["name"])["deployment"]
        with _tenant_config_patched(
            tenant["namespace"],
            {"apiKeys": {"maxExpirationDays": TENANT_MAX_EXPIRATION_DAYS}},
            "/spec/apiKeys",
            settle_deployment=deployment,
            settle_env=API_KEY_MAX_EXPIRATION_ENV,
        ):
            _wait_for_env_rollout(
                deployment,
                INFRA_NAMESPACE,
                API_KEY_MAX_EXPIRATION_ENV,
                str(TENANT_MAX_EXPIRATION_DAYS),
            )
            # Sibling tenant should not have the policy and should accept 30-day keys
            response = create_api_key_at(
                sibling["base_url"],
                token,
                f"e2e-mt-sibling-policy-{uuid.uuid4().hex[:6]}",
                subscription=tenant_key_governance,
                expires_in=f"{EXCEEDS_TENANT_WITHIN_DEFAULT_DAYS * HOURS_PER_DAY}h",
            )
            assert response.status_code in (200, 201), (
                f"Sibling tenant should not inherit the {TENANT_MAX_EXPIRATION_DAYS}-day policy; "
                f"minting a {EXCEEDS_TENANT_WITHIN_DEFAULT_DAYS}-day key returned {response.status_code}"
            )
