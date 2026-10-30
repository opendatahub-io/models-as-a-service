"""E2E: compatibility for legacy-shaped external models and legacy IPP.

P0 scenarios (runtime halves — upgrade-context preservation is
tracked by the separate upgrade-tests task):

4. Objects without gatewayRefs follow the recorded compatibility behavior:
   tenant-local resolution keeps the legacy resource contract, and adding
   gatewayRefs that name only the model's own tenant recreates nothing.
5. Legacy IPP behavior remains compatible during legacy/Praxis coexistence:
   with the default tenant explicitly on ipp, user-namespace/shared AGC
   models are ignored by the legacy IPP controller (no route on the default
   gateway, no IPP status writes, shared provider ownership stays defined),
   and the MaasTenantConfig backend selection / cleanup-claim handoff works
   in both directions.

The coexistence test is serial (it flips the default tenant's payload backend)
and gated on TENANT_EXTERNAL_MODEL_LEGACY_IPP_E2E=true because it requires the MaaS IPP
image pin containing the legacy-controller exclusion guard.
"""

from __future__ import annotations

import logging
import os
import time
import uuid

import pytest
from multitenancy_helpers import (
    DEFAULT_GATEWAY_NAME,
    GATEWAY_NAMESPACE,
    cleanup_discovery_case,
    delete_namespace_best_effort,
    ensure_payload_processing_type_on_tenant_config,
    extproc_deployment_uses_praxis,
    get_json_or_none,
    new_named_tenant_case,
    wait_until,
)
from tenant_external_model_helpers import (
    EXTERNAL_MODEL_KIND,
    TenantExternalModelResources,
    assert_served_by_provider,
    attachment_httproutes,
    bootstrap_external_model_tenant,
    destination_rules_referencing,
    external_model_route_name,
    gateway_ref,
    infer_external_chat,
    new_admitted_user_namespace,
    patch_external_model_gateway_refs,
    payload_processing_annotation,
    provider_external_name_service,
    provision_external_model_stack,
    require_legacy_ipp_coexistence_support,
    restore_payload_processing_annotation,
    serve_external_model_in_tenant,
    wait_for_attachment_httproute,
    wait_for_gateway_attachment_ready,
)
from test_helper import RECONCILE_WAIT, _ns

log = logging.getLogger(__name__)

# Serialized with tenant_ipp until the legacy IPP controllers skip
# gatewayRefs ExternalModels (image pin pending): without that guard they
# adopt this suite's models and flap status.httpRouteName.
pytestmark = pytest.mark.xdist_group("tenant_ipp")

DEFAULT_BACKEND_SWITCH_TIMEOUT = int(os.environ.get("E2E_TENANT_BACKEND_SWITCH_TIMEOUT", "420"))


def _serve_in_tenant(resources: TenantExternalModelResources, case: dict, model: dict) -> dict:
    access = serve_external_model_in_tenant(
        case,
        model["name"],
        model["namespace"],
        policy_name=f"{model['name']}-access",
        subscription_name=f"{model['name']}-sub",
    )
    resources.track("maasauthpolicy", access["policy"], case["tenant_ns"])
    resources.track("maassubscription", access["subscription"], case["tenant_ns"])
    return access


def _wait_model_generation_observed(name: str, namespace: str) -> None:
    """Wait until the AGC has observed the model's current generation.

    Attachment readiness can return on the first poll after a spec patch —
    the status still reports the previous (already Ready) generation — so a
    comparison taken right after `wait_for_gateway_attachment_ready` can
    predate the controller ever reconciling the new spec.
    """

    def _check() -> bool:
        model = get_json_or_none(EXTERNAL_MODEL_KIND, name, namespace) or {}
        metadata = model.get("metadata") or {}
        status = model.get("status") or {}
        generation = metadata.get("generation") or 0
        observed = status.get("observedGeneration") or 0
        assert observed >= generation, (
            f"AGC has not observed {namespace}/{name} generation {generation} "
            f"(observedGeneration={observed})"
        )
        return True

    wait_until(
        _check,
        max(RECONCILE_WAIT * 3, 60),
        f"observedGeneration of {namespace}/{name}",
    )


# ─── P0 #4 (runtime half): omitted gatewayRefs ──────────────────────────────


class TestOmittedGatewayRefsUnchanged:
    """A legacy-shaped model (no gatewayRefs) in the tenant namespace keeps
    the legacy resource contract, and adding own-tenant gatewayRefs recreates
    nothing."""

    @pytest.fixture(scope="class")
    def legacy_shaped_model(self, tenant_external_model_tenants):
        tenant_a = tenant_external_model_tenants["tenant_a"]
        resources = TenantExternalModelResources()
        try:
            model = provision_external_model_stack(
                resources,
                name=f"e2e-cmp-legacy-{uuid.uuid4().hex[:6]}",
                namespace=tenant_a["tenant_ns"],
                gateway_refs=None,
            )
            access = _serve_in_tenant(resources, tenant_a, model)
            # The route lives in the model (tenant) namespace, attached to the
            # tenant's gateway via tenant-local resolution. The AGC names it
            # external-model-<model> — its own naming, not the <model> name the
            # pre-AGC legacy flow used. Scoped by model name: the conflicts
            # module also places models in this tenant namespace.
            routes = wait_for_attachment_httproute(
                model["namespace"], tenant_a["gateway_name"], model_name=model["name"]
            )
            yield {"model": model, "access": access, "routes": routes}
        finally:
            resources.cleanup()

    def test_omitted_gateway_refs_serve_via_tenant_local_resolution(
        self, tenant_external_model_tenants, legacy_shaped_model
    ):
        tenant_a = tenant_external_model_tenants["tenant_a"]
        model = legacy_shaped_model["model"]
        route_names = [route["metadata"]["name"] for route in legacy_shaped_model["routes"]]
        expected_route = external_model_route_name(model["name"])
        assert expected_route in route_names, (
            f"legacy-shaped model must be served by the AGC route {expected_route} "
            f"(tenant-local resolution keeps the AGC naming, not the legacy "
            f"'{model['name']}'), got {route_names}"
        )
        response = infer_external_chat(
            tenant_a["gateway_url"],
            model["namespace"],
            model["client_name"],
            legacy_shaped_model["access"]["api_key"],
            path_tag=model["path_tag"],
        )
        assert_served_by_provider(response, model["path_tag"], credential=model["credential"])

    def test_adding_own_tenant_gateway_refs_recreates_nothing(
        self, tenant_external_model_tenants, legacy_shaped_model
    ):
        tenant_a = tenant_external_model_tenants["tenant_a"]
        model = legacy_shaped_model["model"]
        gateway = tenant_a["gateway_name"]

        def _snapshot() -> dict:
            routes = attachment_httproutes(model["namespace"], gateway)
            return {
                "route_identities": {
                    route["metadata"]["name"]: (
                        route["metadata"]["uid"],
                        route["metadata"].get("labels") or {},
                    )
                    for route in routes
                },
                # AGC keeps the provider DestinationRules in the Gateway
                # namespace, so that is where they are observable.
                "destination_rule_uids": {
                    rule["metadata"]["uid"]
                    for rule in destination_rules_referencing(model["name"], GATEWAY_NAMESPACE)
                },
            }

        before = _snapshot()
        expected_route = external_model_route_name(model["name"])
        assert expected_route in before["route_identities"], (
            f"AGC route {expected_route} not found before the gatewayRefs patch: "
            f"{sorted(before['route_identities'])}"
        )
        assert before["destination_rule_uids"], (
            f"provider DestinationRule for {model['name']} must exist in "
            f"{GATEWAY_NAMESPACE} while the model is serving"
        )

        # Adding gatewayRefs that name only the model's own tenant must not
        # recreate anything: same routes, same uids, same labels. Wait for
        # the AGC to observe the new generation first — attachment readiness
        # can return on the first poll while the status still reflects the
        # previous (already Ready) generation, and a recreation a few
        # seconds later would slip through an immediate comparison.
        patch_external_model_gateway_refs(model["name"], model["namespace"], [gateway_ref(gateway)])
        _wait_model_generation_observed(model["name"], model["namespace"])
        wait_for_gateway_attachment_ready(model["name"], model["namespace"], gateway)

        # Then require the comparison to HOLD across a settle window (like
        # test_nothing_published_into_tenant): the home attachment must not
        # recreate the route at all, so equality must hold, not converge.
        deadline = time.time() + RECONCILE_WAIT
        while True:
            after = _snapshot()
            assert after["route_identities"] == before["route_identities"], (
                "adding own-tenant gatewayRefs recreated or relabeled HTTPRoutes: "
                f"{before['route_identities']} -> {after['route_identities']}"
            )
            # The AGC keys the provider DestinationRule by provider and
            # tenant only — the gateway appears in labels, applied in
            # place — so the home attachment must not change the rule's
            # identity. A uid change here is an AGC regression.
            assert after["destination_rule_uids"] == before["destination_rule_uids"], (
                "adding own-tenant gatewayRefs recreated DestinationRules: "
                f"{before['destination_rule_uids']} -> {after['destination_rule_uids']}"
            )
            if time.time() >= deadline:
                break
            time.sleep(5)
        response = infer_external_chat(
            tenant_a["gateway_url"],
            model["namespace"],
            model["client_name"],
            legacy_shaped_model["access"]["api_key"],
            path_tag=model["path_tag"],
        )
        assert_served_by_provider(response, model["path_tag"], credential=model["credential"])


# ─── P0 #5: legacy IPP coexistence ──────────────────────────────────────────


def _default_payload_processing_is_praxis() -> bool:
    return extproc_deployment_uses_praxis("payload-processing", GATEWAY_NAMESPACE)


def _wait_default_backend(*, praxis: bool, timeout: int = DEFAULT_BACKEND_SWITCH_TIMEOUT) -> None:
    """Wait for the default tenant's payload-processing backend selection."""

    def _check() -> bool:
        deployment = get_json_or_none("deployment", "payload-processing", GATEWAY_NAMESPACE)
        assert deployment is not None, "default payload-processing deployment not found"
        current = _default_payload_processing_is_praxis()
        assert current == praxis, f"default payload-processing praxis={current}, expected {praxis}"
        # A spec-only check can pass while the switch is still rolling out;
        # require a live replica so the selected backend has actually started.
        status = deployment.get("status") or {}
        assert status.get("availableReplicas", 0) >= 1, (
            "default payload-processing deployment has no available replicas"
        )
        return True

    wait_until(
        _check,
        timeout,
        f"default payload-processing backend to become {'praxis' if praxis else 'legacy IPP'}",
    )


def attachment_status_snapshot(model: dict) -> dict:
    """Stable comparable snapshot of a model's attachment status.

    Captures BOTH condition shapes on every call: the model's top-level
    conditions and each per-gateway ``status.gateways`` entry. AGC builds
    that report aggregate status only leave ``status.gateways`` empty, and
    a legacy-controller write may touch either shape — including both in
    one snapshot means a change to one cannot hide behind the other.
    """
    external_model = get_json_or_none(EXTERNAL_MODEL_KIND, model["name"], model["namespace"]) or {}
    status = external_model.get("status") or {}
    snapshot = {
        ("aggregate", ""): sorted(
            (c.get("type"), c.get("status"), c.get("reason")) for c in status.get("conditions") or []
        )
    }
    for entry in status.get("gateways") or []:
        snapshot[(entry.get("name"), entry.get("namespace"))] = sorted(
            (c.get("type"), c.get("status"), c.get("reason"))
            for c in entry.get("conditions") or []
        )
    return snapshot


@pytest.mark.serial
class TestLegacyIppCoexistence:
    """With the default tenant on ipp, the legacy controller ignores
    new-shaped AGC models and the backend handoff works.

    Serial: flips the default tenant's payload-processing backend. Gated on
    TENANT_EXTERNAL_MODEL_LEGACY_IPP_E2E=true (requires the IPP image pin with the
    exclusion guard; the AGC companion deploy removes legacy IPP by default, so
    this also verifies the MaasTenantConfig backend selection redeploys it).
    """

    @pytest.fixture(scope="class")
    def coexistence(self, gateway_url):
        require_legacy_ipp_coexistence_support()
        default_tenant_ns = _ns()
        if not _default_payload_processing_is_praxis():
            pytest.skip(
                "default tenant payload-processing is not praxis; the coexistence "
                "handoff test needs a praxis starting point"
            )
        # Capture the default tenant's original payload-processing annotation
        # (including its absence) so teardown restores the cluster state
        # exactly: praxis may have been selected by cluster default, and
        # leaving an explicit override behind would change that selection.
        original_payload_annotation = payload_processing_annotation(default_tenant_ns)

        # An AGC-served model in a user namespace on a praxis tenant gateway.
        # Short tenant name: AGC per-tenant resource naming caps tenant names
        # at 20 characters (see MAX_AITENANT_NAME_LENGTH).
        case_b = new_named_tenant_case("e2e-extmod-i")
        resources = TenantExternalModelResources()
        user_ns = new_admitted_user_namespace("e2e-extmod-ipp-user", case_b["gateway_name"])
        try:
            bootstrap_external_model_tenant(case_b)
            model = provision_external_model_stack(
                resources,
                name=f"e2e-ipp-coex-{uuid.uuid4().hex[:6]}",
                namespace=user_ns,
                gateway_refs=[gateway_ref(case_b["gateway_name"])],
            )
            access = _serve_in_tenant(resources, case_b, model)

            wait_for_gateway_attachment_ready(model["name"], model["namespace"], case_b["gateway_name"])
            provider_service = get_json_or_none(
                "service", provider_external_name_service(model["provider"]), model["namespace"]
            )
            status_before = attachment_status_snapshot(model)

            # Flip the default tenant to legacy IPP (backend selection +
            # cleanup/claim handoff) and wait for the legacy stack.
            ensure_payload_processing_type_on_tenant_config(default_tenant_ns, "ipp")
            _wait_default_backend(praxis=False)
            # A live legacy replica is necessary but not sufficient for the
            # controller to have acted on the default tenant; settle before
            # the negative assertions so absence checks are meaningful.
            time.sleep(RECONCILE_WAIT)

            yield {
                "case_b": case_b,
                "model": model,
                "access": access,
                "default_gateway_url": gateway_url,
                "status_before": status_before,
                "provider_service_uid": (provider_service or {}).get("metadata", {}).get("uid"),
            }
        finally:
            try:
                # Claim handoff back to the original backend selection (the
                # starting backend is praxis — the skip above enforces it —
                # so praxis must come back either way); a failure here
                # surfaces as a fixture teardown error instead of poisoning
                # later tests.
                restore_payload_processing_annotation(default_tenant_ns, original_payload_annotation)
                _wait_default_backend(praxis=True)
            finally:
                resources.cleanup()
                delete_namespace_best_effort(user_ns)
                cleanup_discovery_case(case_b)

    def test_no_route_on_default_gateway(self, coexistence):
        """The legacy IPP controller does not serve the new-shaped model on
        the default gateway (legacy-controller exclusion guard)."""
        model = coexistence["model"]
        assert attachment_httproutes(model["namespace"], DEFAULT_GATEWAY_NAME) == [], (
            f"legacy IPP must not attach a route for new-shaped model "
            f"{model['namespace']}/{model['name']} to the default gateway"
        )

    def test_not_served_through_default_gateway(self, coexistence):
        model = coexistence["model"]
        response = infer_external_chat(
            coexistence["default_gateway_url"],
            model["namespace"],
            model["client_name"],
            coexistence["access"]["api_key"],
            path_tag=model["path_tag"],
            retries=1,
        )
        assert response.status_code in (401, 403, 404), (
            f"new-shaped AGC model must not be served on the default gateway, got "
            f"{response.status_code}"
        )

    def test_no_ipp_status_writes(self, coexistence):
        """The AGC per-gateway status is not overwritten by the legacy
        controller while the default tenant runs ipp."""
        model = coexistence["model"]
        status_after = attachment_status_snapshot(model)
        assert status_after == coexistence["status_before"], (
            "legacy IPP overwrote the AGC status.gateways results: "
            f"{coexistence['status_before']} -> {status_after}"
        )

    def test_shared_provider_ownership_stays_defined(self, coexistence):
        """The provider's reconciled Service keeps its identity (the legacy
        controller neither claims nor deletes it)."""
        model = coexistence["model"]
        service = get_json_or_none(
            "service", provider_external_name_service(model["provider"]), model["namespace"]
        )
        expected_uid = coexistence["provider_service_uid"]
        assert service is not None, "provider Service disappeared during legacy/Praxis coexistence"
        if expected_uid:
            assert service["metadata"]["uid"] == expected_uid, (
                "provider Service was recreated (ownership handoff broke)"
            )

    def test_agc_tenant_gateway_still_serves(self, coexistence):
        """The AGC/Praxis path on the tenant gateway is unaffected by the
        default tenant's legacy backend."""
        model = coexistence["model"]
        case_b = coexistence["case_b"]
        response = infer_external_chat(
            case_b["gateway_url"],
            model["namespace"],
            model["client_name"],
            coexistence["access"]["api_key"],
            path_tag=model["path_tag"],
        )
        assert_served_by_provider(response, model["path_tag"], credential=model["credential"])
