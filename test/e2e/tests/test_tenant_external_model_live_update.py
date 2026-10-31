"""E2E: live ExternalModel edits re-route through the gateway without a restart.

RHOAIENG-101736 (gap G5/G7): an edit to a serving ExternalModel must take effect
at runtime (no redeploy/pod action), and a removed model must stop serving (never
a stale 200). Re-points a model's provider ref A->B in place and polls inference
until the injected credential flips; then deletes the model and polls until it
stops serving. Uses the #1713 single-gateway/attachment helpers with the httpbin
header-echo endpoint (the echo reflects the injected credential).
"""

from __future__ import annotations

import logging
import uuid

import pytest
from multitenancy_helpers import (
    delete_namespace_best_effort,
    redact_sensitive,
    wait_until,
)
from tenant_external_model_helpers import (
    EXTERNAL_MODEL_KIND,
    TenantExternalModelResources,
    apply_credential_secret,
    apply_external_model,
    apply_external_provider,
    assert_served_by_provider,
    gateway_ref,
    infer_external_chat,
    new_admitted_user_namespace,
    provision_external_model_stack,
    serve_external_model_in_tenant,
)
from test_helper import RECONCILE_WAIT, _delete_cr

log = logging.getLogger(__name__)

pytestmark = pytest.mark.xdist_group("tenant_ipp")

# A live edit must converge within: controller reconcile + routing-overlay
# projection + intelligent_route debounce (500ms) + credential-copy delivery.
LIVE_UPDATE_TIMEOUT = max(RECONCILE_WAIT * 15, 120)


@pytest.fixture(scope="module")
def live_update_env(tenant_external_model_tenants):
    tenant = tenant_external_model_tenants["tenant_b"]
    user_ns = new_admitted_user_namespace("e2e-extmod-live-user", tenant["gateway_name"])
    resources = TenantExternalModelResources()
    try:
        yield {"tenant": tenant, "user_ns": user_ns, "resources": resources}
    finally:
        resources.cleanup()
        delete_namespace_best_effort(user_ns)


class TestLiveExternalModelUpdate:
    """A serving ExternalModel re-routes on edit and stops on delete, no restart."""

    @pytest.fixture(scope="class")
    def served(self, live_update_env):
        env = live_update_env
        tenant = env["tenant"]
        model = provision_external_model_stack(
            env["resources"],
            name=f"e2e-live-{uuid.uuid4().hex[:8]}",
            namespace=env["user_ns"],
            gateway_refs=[gateway_ref(tenant["gateway_name"])],
        )
        provider_b = f"{model['name']}-provider-b"
        secret_b = f"{provider_b}-cred"
        credential_b = f"sk-extmod-live-b-{uuid.uuid4().hex[:8]}"
        apply_credential_secret(secret_b, env["user_ns"], credential_b)
        env["resources"].track("secret", secret_b, env["user_ns"])
        apply_external_provider(provider_b, env["user_ns"], credential_secret=secret_b)
        env["resources"].track(
            "externalproviders.inference.opendatahub.io", provider_b, env["user_ns"]
        )
        access = serve_external_model_in_tenant(
            tenant,
            model["name"],
            model["namespace"],
            policy_name=f"{model['name']}-access",
            subscription_name=f"{model['name']}-sub",
        )
        env["resources"].track("maasauthpolicy", access["policy"], tenant["tenant_ns"])
        env["resources"].track("maassubscription", access["subscription"], tenant["tenant_ns"])
        return {
            "model": model,
            "api_key": access["api_key"],
            "provider_b": provider_b,
            "credential_b": credential_b,
        }

    def test_live_provider_repoint_and_removal_take_effect_without_restart(self, live_update_env, served):
        tenant = live_update_env["tenant"]
        model = served["model"]
        credential_a = model["credential"]
        credential_b = served["credential_b"]
        assert credential_a != credential_b

        def infer():
            return infer_external_chat(
                tenant["gateway_url"],
                model["namespace"],
                model["name"],
                served["api_key"],
                path_tag=model["path_tag"],
                retries=1,
            )

        log.info("Baseline: asserting %s serves provider A", model["name"])
        assert_served_by_provider(infer(), model["path_tag"], credential=credential_a)

        log.info("Re-pointing %s provider ref A->B in place", model["name"])
        apply_external_model(
            model["name"],
            model["namespace"],
            provider_name=served["provider_b"],
            gateway_refs=[gateway_ref(tenant["gateway_name"])],
            path_tag=model["path_tag"],
        )

        wait_until(
            lambda: (r := infer()).status_code == 200 and credential_b in r.text,
            timeout=LIVE_UPDATE_TIMEOUT,
            what="re-pointed model serves provider B's credential",
        )
        rerouted = infer()
        assert_served_by_provider(rerouted, model["path_tag"], credential=credential_b)
        assert credential_a not in rerouted.text, (
            f"stale provider A credential still injected: {redact_sensitive(rerouted.text)}"
        )

        log.info("Deleting %s; expecting it to stop serving live", model["name"])
        _delete_cr(EXTERNAL_MODEL_KIND, model["name"], model["namespace"])
        wait_until(
            lambda: infer().status_code != 200,
            timeout=LIVE_UPDATE_TIMEOUT,
            what="deleted model stops serving (no stale 200)",
        )
