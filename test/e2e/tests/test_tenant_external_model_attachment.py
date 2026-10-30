"""E2E: explicit gateway attachment for user-namespace external models.

P0 scenarios:

1. A model in a user namespace explicitly attached to a non-default Praxis
   tenant gateway serves inference through that gateway using its credential
   Secret from the user namespace (delivered as a controller-owned copy).
   Other unselected gateways do not serve it.
2. Removing an attachment or deleting a model removes stale serving
   configuration and unreferenced credential copies while preserving
   unrelated attachments/models.
3. An attachment to a gateway whose allowedRoutes do not admit the model
   namespace is not served, gets no DestinationRule or credential copy, and
   reports NotAllowedByListeners.

Shares the tenant_external_model session tenants (see conftest.tenant_external_model_tenants);
this module creates its own user namespaces with unique names.
"""

from __future__ import annotations

import logging
import time
import uuid

import pytest
from multitenancy_helpers import (
    GATEWAY_NAMESPACE,
    delete_namespace_best_effort,
    redact_sensitive,
)
from tenant_external_model_helpers import (
    EXTERNAL_MODEL_KIND,
    TenantExternalModelResources,
    assert_served_by_provider,
    attachment_httproutes,
    credential_copy_in,
    destination_rules_referencing,
    gateway_ref,
    httproute_parent_conditions,
    infer_external_chat,
    new_admitted_user_namespace,
    new_unadmitted_namespace,
    patch_external_model_gateway_refs,
    provider_external_name_service,
    provision_external_model_stack,
    serve_external_model_in_tenant,
    wait_for_attachment_httproute,
    wait_for_credential_copy,
    wait_for_gateway_attachment_absent,
    wait_for_gateway_attachment_ready,
    wait_for_gateway_attachment_reason,
    wait_for_no_attachment_httproute,
    wait_for_no_credential_copy,
    wait_for_no_destination_rules_referencing,
)
from test_helper import RECONCILE_WAIT, _delete_cr, _get_cr

log = logging.getLogger(__name__)

# Serialized with tenant_ipp until the legacy IPP controllers skip
# gatewayRefs ExternalModels (image pin pending): without that guard they
# adopt this suite's models and flap status.httpRouteName.
pytestmark = pytest.mark.xdist_group("tenant_ipp")


@pytest.fixture(scope="module")
def attachment_env(tenant_external_model_tenants):
    """User namespaces (admitted + deliberately unadmitted) on the shared tenants."""
    tenant_a = tenant_external_model_tenants["tenant_a"]
    tenant_b = tenant_external_model_tenants["tenant_b"]
    user_ns = new_admitted_user_namespace(
        "e2e-extmod-att-user", tenant_a["gateway_name"], tenant_b["gateway_name"]
    )
    unadmitted_ns = new_unadmitted_namespace("e2e-extmod-att-noaccess")
    resources = TenantExternalModelResources()
    try:
        yield {
            "tenant_a": tenant_a,
            "tenant_b": tenant_b,
            "user_ns": user_ns,
            "unadmitted_ns": unadmitted_ns,
            "resources": resources,
        }
    finally:
        resources.cleanup()
        delete_namespace_best_effort(user_ns)
        delete_namespace_best_effort(unadmitted_ns)


def _serve_in_tenant(env: dict, case: dict, model: dict, **kwargs) -> dict:
    access = serve_external_model_in_tenant(
        case,
        model["name"],
        model["namespace"],
        policy_name=f"{model['name']}-access",
        subscription_name=f"{model['name']}-sub",
        **kwargs,
    )
    env["resources"].track("maasauthpolicy", access["policy"], case["tenant_ns"])
    env["resources"].track("maassubscription", access["subscription"], case["tenant_ns"])
    return access


def _assert_not_served(response) -> None:
    assert response.status_code in (401, 403, 404), (
        f"expected the model to not be served, got {response.status_code}: "
        f"{redact_sensitive(response.text)}"
    )


# ─── P0 #1: user-namespace model on a non-default tenant gateway ────────────


class TestUserNamespaceAttachmentServing:
    """A user-ns model attached to tenant B's gateway serves through it only."""

    @pytest.fixture(scope="class")
    def attached_model(self, attachment_env):
        env = attachment_env
        tenant_b = env["tenant_b"]
        model = provision_external_model_stack(
            env["resources"],
            name=f"e2e-att-serve-{uuid.uuid4().hex[:6]}",
            namespace=env["user_ns"],
            gateway_refs=[gateway_ref(tenant_b["gateway_name"])],
        )
        access = _serve_in_tenant(env, tenant_b, model)
        return {"model": model, "access": access}

    def test_attachment_status_ready(self, attachment_env, attached_model):
        """The attachment to tenant B's gateway reports serving readiness
        (per-gateway status.gateways entry, or the model's aggregate Ready
        when the AGC reports aggregate status only)."""
        env = attachment_env
        model = attached_model["model"]
        wait_for_gateway_attachment_ready(
            model["name"], model["namespace"], env["tenant_b"]["gateway_name"]
        )

    def test_one_httproute_in_model_namespace(self, attachment_env, attached_model):
        """The attachment's HTTPRoute lives in the model namespace and routes to
        the provider Service created there."""
        env = attachment_env
        model = attached_model["model"]
        tenant_b = env["tenant_b"]
        # Scope by model name: the lifecycle and listener classes share this
        # user namespace and gateway, so an unscoped wait could pass on one
        # of their routes.
        routes = wait_for_attachment_httproute(
            model["namespace"], tenant_b["gateway_name"], model_name=model["name"]
        )
        assert len(routes) == 1, (
            f"expected exactly one HTTPRoute attached to {tenant_b['gateway_name']} "
            f"in {model['namespace']}, got {[r['metadata']['name'] for r in routes]}"
        )
        route = routes[0]
        assert route["metadata"]["namespace"] == model["namespace"]
        backend_names = [
            backend.get("name")
            for rule in route.get("spec", {}).get("rules", [])
            for backend in rule.get("backendRefs", [])
        ]
        provider_service = provider_external_name_service(model["provider"])
        assert provider_service in backend_names, (
            f"HTTPRoute backendRefs {backend_names} do not reference the provider's "
            f"ExternalName Service {provider_service}"
        )

    def test_credential_copy_delivered_to_tenant(self, attachment_env, attached_model):
        """The user-namespace credential is delivered into the tenant namespace
        as a controller-owned copy with the same content."""
        env = attachment_env
        model = attached_model["model"]
        tenant_b = env["tenant_b"]
        copy = wait_for_credential_copy(
            tenant_b["tenant_ns"], model["secret_name"], model["secret_namespace"]
        )
        assert copy["metadata"]["namespace"] == tenant_b["tenant_ns"]

    def test_unselected_gateway_not_serving(self, attachment_env, attached_model):
        """Tenant A's gateway has no route, no credential copy, and does not
        serve the model."""
        env = attachment_env
        model = attached_model["model"]
        tenant_a = env["tenant_a"]
        assert attachment_httproutes(model["namespace"], tenant_a["gateway_name"]) == [], (
            "unselected gateway must not have an HTTPRoute for the model"
        )
        assert (
            credential_copy_in(tenant_a["tenant_ns"], model["secret_name"], model["secret_namespace"])
            is None
        ), "unselected tenant must not receive a credential copy"
        response = infer_external_chat(
            tenant_a["gateway_url"],
            model["namespace"],
            model["client_name"],
            attached_model["access"]["api_key"],
            # The same request that serves on tenant B, with no retries: A
            # has no route, so the refusal is immediate and stable.
            path_tag=model["path_tag"],
            retries=1,
        )
        _assert_not_served(response)

    def test_inference_serves_with_user_namespace_credential(self, attachment_env, attached_model):
        """Real inference through tenant B's gateway reaches the provider with
        the user-namespace credential injected."""
        env = attachment_env
        model = attached_model["model"]
        tenant_b = env["tenant_b"]
        response = infer_external_chat(
            tenant_b["gateway_url"],
            model["namespace"],
            model["client_name"],
            attached_model["access"]["api_key"],
            path_tag=model["path_tag"],
        )
        assert_served_by_provider(response, model["path_tag"], credential=model["credential"])


# ─── P0 #3: lifecycle cleanup ───────────────────────────────────────────────


class TestAttachmentLifecycleCleanup:
    """Detach and delete remove only their own serving configuration."""

    @pytest.fixture(scope="class")
    def unrelated_model(self, attachment_env):
        """A bystander model on tenant B's gateway that must survive cleanup."""
        env = attachment_env
        tenant_b = env["tenant_b"]
        model = provision_external_model_stack(
            env["resources"],
            name=f"e2e-att-unrel-{uuid.uuid4().hex[:6]}",
            namespace=env["user_ns"],
            gateway_refs=[gateway_ref(tenant_b["gateway_name"])],
        )
        access = _serve_in_tenant(env, tenant_b, model)
        return {"model": model, "access": access}

    def test_detach_removes_only_that_attachment(self, attachment_env, unrelated_model):
        """Removing tenant A's attachment removes A's route and (now
        unreferenced) credential copy while tenant B keeps serving.

        The model starts attached to both gateways, but the AGC reports
        ``status.httpRouteName`` only for single-gateway models (the same
        gap the shared module's MaaS-route gate documents), so the MaaS
        subscription/key stack is built after the detach leaves one
        gateway; before that the attachment is verified at the
        route/credential level on both tenants."""
        env = attachment_env
        tenant_a = env["tenant_a"]
        tenant_b = env["tenant_b"]
        model = provision_external_model_stack(
            env["resources"],
            name=f"e2e-att-detach-{uuid.uuid4().hex[:6]}",
            namespace=env["user_ns"],
            gateway_refs=[gateway_ref(tenant_a["gateway_name"]), gateway_ref(tenant_b["gateway_name"])],
        )

        # Baseline: both attachments are live at the serving-config level.
        # AGC puts the provider DestinationRules in the Gateway namespace,
        # labeled with each attachment's gateway — capture both per gateway.
        for case in (tenant_a, tenant_b):
            wait_for_gateway_attachment_ready(model["name"], model["namespace"], case["gateway_name"])
            wait_for_attachment_httproute(
                model["namespace"], case["gateway_name"], model_name=model["name"]
            )
            wait_for_credential_copy(case["tenant_ns"], model["secret_name"], model["secret_namespace"])
            assert destination_rules_referencing(
                model["name"], GATEWAY_NAMESPACE, also_matching=(case["gateway_name"],)
            ), f"provider DestinationRule for {model['name']} on {case['gateway_name']} must exist while attached"

        tenant_b_rule_uids = {
            rule["metadata"]["uid"]
            for rule in destination_rules_referencing(
                model["name"], GATEWAY_NAMESPACE, also_matching=(tenant_b["gateway_name"],)
            )
        }

        patch_external_model_gateway_refs(
            model["name"], model["namespace"], [gateway_ref(tenant_b["gateway_name"])]
        )

        wait_for_gateway_attachment_absent(model["name"], model["namespace"], tenant_a["gateway_name"])
        wait_for_no_attachment_httproute(
            model["namespace"], tenant_a["gateway_name"], model_name=model["name"]
        )
        wait_for_no_credential_copy(
            tenant_a["tenant_ns"], model["secret_name"], model["secret_namespace"]
        )
        wait_for_no_destination_rules_referencing(
            model["name"], GATEWAY_NAMESPACE, also_matching=(tenant_a["gateway_name"],)
        )
        # The remaining attachment's DestinationRule is untouched.
        assert {
            rule["metadata"]["uid"]
            for rule in destination_rules_referencing(
                model["name"], GATEWAY_NAMESPACE, also_matching=(tenant_b["gateway_name"],)
            )
        } == tenant_b_rule_uids, "detaching tenant A must not recreate tenant B's DestinationRule"

        # The remaining attachment is untouched and now serves end to end:
        # with a single gateway the AGC reports httpRouteName, so the MaaS
        # subscription/key stack completes.
        wait_for_gateway_attachment_ready(model["name"], model["namespace"], tenant_b["gateway_name"])
        wait_for_credential_copy(tenant_b["tenant_ns"], model["secret_name"], model["secret_namespace"])
        access_b = _serve_in_tenant(env, tenant_b, model)
        response = infer_external_chat(
            tenant_b["gateway_url"],
            model["namespace"],
            model["client_name"],
            access_b["api_key"],
            path_tag=model["path_tag"],
        )
        assert_served_by_provider(response, model["path_tag"], credential=model["credential"])

    def test_model_delete_removes_serving_config(self, attachment_env, unrelated_model):
        """Deleting a model removes all of its serving configuration while an
        unrelated model on the same gateway keeps serving.

        Like the detach test, the deleted model is attached to two
        gateways, so no MaaS stack is built for it (single-gateway
        httpRouteName gap) — its cleanup is proven at the route/credential
        level; the bystander proves serving survives the deletion."""
        env = attachment_env
        tenant_a = env["tenant_a"]
        tenant_b = env["tenant_b"]
        model = provision_external_model_stack(
            env["resources"],
            name=f"e2e-att-delete-{uuid.uuid4().hex[:6]}",
            namespace=env["user_ns"],
            gateway_refs=[gateway_ref(tenant_a["gateway_name"]), gateway_ref(tenant_b["gateway_name"])],
        )

        # Both attachments are live before the delete.
        for case in (tenant_a, tenant_b):
            wait_for_gateway_attachment_ready(model["name"], model["namespace"], case["gateway_name"])
            wait_for_credential_copy(case["tenant_ns"], model["secret_name"], model["secret_namespace"])
            assert destination_rules_referencing(
                model["name"], GATEWAY_NAMESPACE, also_matching=(case["gateway_name"],)
            ), f"provider DestinationRule for {model['name']} on {case['gateway_name']} must exist while attached"

        _delete_cr("maasmodelref", model["name"], model["namespace"])
        _delete_cr(EXTERNAL_MODEL_KIND, model["name"], model["namespace"])

        for case in (tenant_a, tenant_b):
            wait_for_no_attachment_httproute(
                model["namespace"], case["gateway_name"], model_name=model["name"]
            )
            wait_for_no_credential_copy(
                case["tenant_ns"], model["secret_name"], model["secret_namespace"]
            )
        # AGC keeps the provider DestinationRules in the Gateway namespace:
        # deleting the model must remove them all, on every attachment.
        wait_for_no_destination_rules_referencing(model["name"], GATEWAY_NAMESPACE)
        assert _get_cr(EXTERNAL_MODEL_KIND, model["name"], model["namespace"]) is None

        # The unrelated model on the same gateway is unaffected.
        unrelated = unrelated_model["model"]
        response = infer_external_chat(
            tenant_b["gateway_url"],
            unrelated["namespace"],
            unrelated["client_name"],
            unrelated_model["access"]["api_key"],
            path_tag=unrelated["path_tag"],
        )
        assert_served_by_provider(response, unrelated["path_tag"], credential=unrelated["credential"])


# ─── P0 #6: gateway-attachment authorization (NotAllowedByListeners) ────────


class TestListenerNotAllowed:
    """A gateway whose allowedRoutes do not admit the model namespace refuses
    the attachment and publishes nothing into the tenant."""

    @pytest.fixture(scope="class")
    def refused_model(self, attachment_env):
        env = attachment_env
        tenant_b = env["tenant_b"]
        return provision_external_model_stack(
            env["resources"],
            name=f"e2e-att-noaccess-{uuid.uuid4().hex[:6]}",
            namespace=env["unadmitted_ns"],
            gateway_refs=[gateway_ref(tenant_b["gateway_name"])],
        )

    def test_attachment_reports_not_allowed_by_listeners(self, attachment_env, refused_model):
        env = attachment_env
        model = refused_model
        wait_for_gateway_attachment_reason(
            model["name"], model["namespace"], env["tenant_b"]["gateway_name"], "NotAllowedByListeners"
        )

    def test_nothing_published_into_tenant(self, attachment_env, refused_model):
        env = attachment_env
        model = refused_model
        tenant_b = env["tenant_b"]
        # Gate on the refusal being reported first: a bare settle window
        # could pass before the controller ever reconciles, making absence
        # trivially true. The test must not rely on its sibling having
        # already waited for the reason.
        wait_for_gateway_attachment_reason(
            model["name"], model["namespace"], tenant_b["gateway_name"], "NotAllowedByListeners"
        )
        # Then require absence to HOLD across a settle window: the
        # attachment must never publish, so absence must hold, not converge.
        # AGC keeps the provider DestinationRules in the Gateway namespace,
        # so that is where "never created" is observable.
        deadline = time.time() + RECONCILE_WAIT
        while True:
            assert (
                credential_copy_in(tenant_b["tenant_ns"], model["secret_name"], model["secret_namespace"])
                is None
            ), "credential copy must not be delivered while the listener refuses the namespace"
            assert not destination_rules_referencing(model["name"], GATEWAY_NAMESPACE), (
                "DestinationRule must not be created while the listener refuses the namespace"
            )
            if time.time() >= deadline:
                break
            time.sleep(5)

    def test_route_not_admitted_and_not_served(self, attachment_env, refused_model):
        env = attachment_env
        model = refused_model
        tenant_b = env["tenant_b"]
        routes = attachment_httproutes(model["namespace"], tenant_b["gateway_name"])
        if routes:
            # AGC creates the attachment's HTTPRoute in the model namespace
            # first; the gateway must refuse to admit it.
            for route in routes:
                conditions = httproute_parent_conditions(route, tenant_b["gateway_name"])
                accepted = next((c for c in conditions if c.get("type") == "Accepted"), None)
                assert accepted is not None and accepted.get("status") == "False", (
                    f"route {route['metadata']['name']} must not be admitted: {conditions}"
                )
        response = infer_external_chat(
            tenant_b["gateway_url"],
            model["namespace"],
            model["client_name"],
            api_key=f"sk-extmod-unused-{uuid.uuid4().hex[:8]}",
            path_tag=model["path_tag"],
            retries=1,
        )
        _assert_not_served(response)
