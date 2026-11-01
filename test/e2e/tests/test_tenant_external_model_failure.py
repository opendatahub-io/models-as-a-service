"""E2E: one tenant's failing external provider must not degrade another tenant.

PR #1713 covers happy-path multi-tenant external-model serving plus per-tenant
auth and quota isolation. This adds the failure-path dimension: when inference
through one tenant's gateway hits an upstream that returns 5xx, another tenant's
independent external model keeps serving through its own gateway/ExtProc.

Each tenant gets its own single-gateway external model (the attachment shape from
test_tenant_external_model_attachment.py, so no shared-model resolvedModelAlias
dependency). The shared header-echo endpoint doubles as the fault injector:
httpbingo's ``/status/503`` returns that status, so routing tenant A's request
there makes its upstream error deterministically while tenant B keeps using the
healthy ``/anything`` echo path.
"""

from __future__ import annotations

import logging
import os
import uuid

import pytest
from multitenancy_helpers import delete_namespace_best_effort, redact_sensitive
from tenant_external_model_helpers import (
    TenantExternalModelResources,
    assert_served_by_provider,
    gateway_ref,
    infer_external_chat,
    new_admitted_user_namespace,
    provision_external_model_stack,
    serve_external_model_in_tenant,
)

log = logging.getLogger(__name__)

# Serialized with the rest of the tenant-external-model suite (shared tenant
# gateways / IPP coexistence) — see test_tenant_external_model_shared.py.
pytestmark = pytest.mark.xdist_group("tenant_ipp")

# httpbin-compatible endpoints return /status/<code> as that status, so routing
# tenant A here fails its upstream deterministically. Override these if
# E2E_EXTERNAL_ENDPOINT does not serve /status/503.
FAILING_UPSTREAM_PATH = os.environ.get("E2E_EXTERNAL_FAILURE_PATH", "/status/503")
FAILING_UPSTREAM_STATUS = int(os.environ.get("E2E_EXTERNAL_FAILURE_STATUS", "503"))


@pytest.fixture(scope="module")
def failure_env(tenant_external_model_tenants):
    tenant_a = tenant_external_model_tenants["tenant_a"]
    tenant_b = tenant_external_model_tenants["tenant_b"]
    user_ns = new_admitted_user_namespace(
        "e2e-extmod-fail-user", tenant_a["gateway_name"], tenant_b["gateway_name"]
    )
    resources = TenantExternalModelResources()
    try:
        yield {
            "tenant_a": tenant_a,
            "tenant_b": tenant_b,
            "user_ns": user_ns,
            "resources": resources,
        }
    finally:
        resources.cleanup()
        delete_namespace_best_effort(user_ns)


def _provision_and_serve(env: dict, case: dict) -> dict:
    """Provision one external model attached to this tenant's gateway and serve it."""
    model = provision_external_model_stack(
        env["resources"],
        name=f"e2e-fail-{uuid.uuid4().hex[:8]}",
        namespace=env["user_ns"],
        gateway_refs=[gateway_ref(case["gateway_name"])],
    )
    access = serve_external_model_in_tenant(
        case,
        model["name"],
        model["namespace"],
        policy_name=f"{model['name']}-access",
        subscription_name=f"{model['name']}-sub",
    )
    env["resources"].track("maasauthpolicy", access["policy"], case["tenant_ns"])
    env["resources"].track("maassubscription", access["subscription"], case["tenant_ns"])
    return {"model": model, "api_key": access["api_key"]}


class TestTenantProviderFailureIsolation:
    """A 5xx from one tenant's upstream must not degrade the other tenant."""

    @pytest.fixture(scope="class")
    def served(self, failure_env):
        """Each tenant gets its own independent external model, served on its gateway."""
        return {
            "a": _provision_and_serve(failure_env, failure_env["tenant_a"]),
            "b": _provision_and_serve(failure_env, failure_env["tenant_b"]),
        }

    def test_upstream_failure_in_one_tenant_does_not_degrade_the_other(self, failure_env, served):
        tenant_a, tenant_b = failure_env["tenant_a"], failure_env["tenant_b"]
        model_a, model_b = served["a"], served["b"]

        def serve(case: dict, served_model: dict, *, retries: int = 6):
            return infer_external_chat(
                case["gateway_url"],
                served_model["model"]["namespace"],
                served_model["model"]["name"],
                served_model["api_key"],
                path_tag=served_model["model"]["path_tag"],
                retries=retries,
            )

        log.info("Baseline: asserting both tenant A and tenant B serve healthy traffic")
        assert_served_by_provider(
            serve(tenant_a, model_a), model_a["model"]["path_tag"], credential=model_a["model"]["credential"]
        )
        assert_served_by_provider(
            serve(tenant_b, model_b), model_b["model"]["path_tag"], credential=model_b["model"]["credential"]
        )

        log.info("Fault injection: routing tenant A to %s (expect %d)", FAILING_UPSTREAM_PATH, FAILING_UPSTREAM_STATUS)
        failing = infer_external_chat(
            tenant_a["gateway_url"],
            model_a["model"]["namespace"],
            model_a["model"]["name"],
            model_a["api_key"],
            provider_path=FAILING_UPSTREAM_PATH,
            retries=1,
        )
        assert failing.status_code == FAILING_UPSTREAM_STATUS, (
            f"expected tenant A's upstream {FAILING_UPSTREAM_STATUS} to propagate, got "
            f"{failing.status_code}: {redact_sensitive(failing.text)}"
        )

        log.info("Isolation: asserting tenant B still serves despite tenant A's 503")
        assert_served_by_provider(
            serve(tenant_b, model_b, retries=1),
            model_b["model"]["path_tag"],
            credential=model_b["model"]["credential"],
        )
