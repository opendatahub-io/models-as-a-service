"""E2E: one external model shared across multiple tenant gateways.

P0 scenario 2:

- One shared model attached to two supported tenant gateways has correct
  routes (one HTTPRoute per attachment, in the model namespace) and
  per-gateway status.
- Requests verify that authorization and quota consumption remain isolated:
  a user authorized in one tenant cannot call the model through the other
  tenant's gateway (both tenants authorize the model, so the cross-tenant
  rejections prove key isolation rather than an absent grant), and
  exhausting tenant A's token budget does not consume tenant B's.

Shares the tenant_external_model session tenants (see conftest.tenant_external_model_tenants);
this module creates its own user namespace with unique names.

The model is served by one MaaSModelRef per tenant (``spec.tenantRef``
pinned): per-gateway status makes the controller's tenant selection require
it for models attached to more than one gateway, and each tenant's
subscription references its own modelRef.

The route/status tests always run. The serving tests (auth and quota
isolation, real inference) additionally need two capabilities, each with its
own skip gate:

- the AGC's per-gateway ``status.gateways`` results: aggregate-status AGC
  builds write ``status.httpRouteName`` for single-attachment models only,
  so the MaaS stack cannot resolve the multi-gateway model's route and those
  tests skip there (see ``_require_maas_route``);
- the client-facing ``spec.modelName`` carried as the model identity: the
  request path and the modelRefs' names disagree here by construction
  (per-tenant refs have distinct names), so path-routed requests only
  authorize once ``status.resolvedModelAlias`` carries the client-facing
  name and maas-api matches pairs against it (see
  ``_require_client_name_alias``). Unknown-key rejection needs no model
  identity resolution and runs with per-gateway status alone.
"""

from __future__ import annotations

import logging
import time
import uuid

import pytest
from multitenancy_helpers import (
    delete_namespace_best_effort,
    get_json_or_none,
    redact_sensitive,
    wait_until,
)
from tenant_external_model_helpers import (
    TenantExternalModelResources,
    assert_returns_usage,
    assert_served_by_provider,
    external_model_reports_maas_route,
    gateway_ref,
    gateway_status_entry,
    infer_external_chat,
    maas_model_ref_alias,
    new_admitted_user_namespace,
    provision_external_model_stack,
    serve_external_model_in_tenant,
    wait_for_attachment_httproute,
    wait_for_gateway_attachment_ready,
)
from test_helper import (
    LLM_D_INFERENCE_SIM_IMAGE,
    _apply_cr,
)

log = logging.getLogger(__name__)

# Serialized with tenant_ipp until the legacy IPP controllers skip
# gatewayRefs ExternalModels (image pin pending): without that guard they
# adopt this suite's models and flap status.httpRouteName.
pytestmark = pytest.mark.xdist_group("tenant_ipp")

# Tenant A gets a tiny budget so quota isolation is observable quickly;
# tenant B gets a large one (mirrors test_tenant_rate_limit_isolation.py).
LIMITED_TOKEN_LIMIT = 3
UNLIMITED_TOKEN_LIMIT = 100

# Token budgets are accounted from usage.total_tokens in inference
# responses, which the header-echo endpoint never returns — so the quota
# model's provider is a dedicated in-cluster simulator instead. The gateway
# envoy verifies provider certificates, so the simulator's Service gets an
# OpenShift service serving certificate (signed by the cluster service CA),
# and the provider points envoy at the CA bundle mounted in every pod via
# spec.config["tls.caCertificates"] — see _usage_provider.
SA_SERVICE_CA = "/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt"
SERVING_CERT_ANNOTATION = "service.beta.openshift.io/serving-cert-secret-name"


@pytest.fixture(scope="module")
def shared_env(tenant_external_model_tenants):
    tenant_a = tenant_external_model_tenants["tenant_a"]
    tenant_b = tenant_external_model_tenants["tenant_b"]
    user_ns = new_admitted_user_namespace(
        "e2e-extmod-shr-user", tenant_a["gateway_name"], tenant_b["gateway_name"]
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


def _serve_in_tenant(env: dict, case: dict, model: dict, **kwargs) -> dict:
    ref_name = model["model_refs"][case["tenant_label_name"]]
    access = serve_external_model_in_tenant(
        case,
        ref_name,
        model["namespace"],
        policy_name=f"{model['name']}-access",
        subscription_name=f"{model['name']}-sub",
        **kwargs,
    )
    env["resources"].track("maasauthpolicy", access["policy"], case["tenant_ns"])
    env["resources"].track("maassubscription", access["subscription"], case["tenant_ns"])
    return access


def _wait_attachments_ready(env: dict, model: dict) -> None:
    for case in (env["tenant_a"], env["tenant_b"]):
        wait_for_gateway_attachment_ready(model["name"], model["namespace"], case["gateway_name"])


def _require_maas_route(model: dict) -> None:
    """Skip when the MaaS stack cannot resolve this multi-gateway model.

    Aggregate-status AGC builds report no per-gateway ``status.gateways``
    results and no ``status.httpRouteName`` for multi-attachment models, so
    maas-controller cannot select a route: the MaaSModelRef never reports an
    endpoint and the subscription TRLP backend stays not-ready, which makes
    per-tenant auth keys and quotas impossible to wire for the shared model.
    """
    if not external_model_reports_maas_route(model["name"], model["namespace"]):
        pytest.skip(
            "AGC reports aggregate model status only (no per-gateway status.gateways "
            "results); the MaaS stack cannot resolve a multi-gateway model's route, so "
            "per-tenant auth/quota serving for a shared model cannot be exercised"
        )


def _require_client_name_alias(model: dict, case: dict) -> None:
    """Skip until the published model identity is the client-facing name.

    The per-tenant stacks go green with per-gateway status alone, but a
    path-routed request carries ``{namespace}/{client-facing name}`` while
    the modelRefs are named after their tenants: requests only authorize
    once the controller publishes ``spec.modelName`` as
    ``status.resolvedModelAlias`` and maas-api matches pairs against it.
    Both halves ship together, so one alias check gates both.
    """
    ref_name = model["model_refs"][case["tenant_label_name"]]
    alias = maas_model_ref_alias(model["namespace"], ref_name)
    if alias != model["client_name"]:
        pytest.skip(
            "client-facing model names are not resolvable through the gateway "
            f"yet: MaaSModelRef {model['namespace']}/{ref_name} "
            f"status.resolvedModelAlias={alias!r} != {model['client_name']!r}"
        )


class TestSharedModelTwoGateways:
    """One model, two attachments: routes, status and serving on both."""

    @pytest.fixture(scope="class")
    def shared_model(self, shared_env):
        env = shared_env
        name = f"e2e-shr-shared-{uuid.uuid4().hex[:6]}"
        model = provision_external_model_stack(
            env["resources"],
            name=name,
            namespace=env["user_ns"],
            gateway_refs=[
                gateway_ref(env["tenant_a"]["gateway_name"]),
                gateway_ref(env["tenant_b"]["gateway_name"]),
            ],
            # The client-facing name deliberately differs from the CRD name:
            # per-tenant modelRefs necessarily have distinct names, so shared
            # requests must resolve through the client-facing identity (the
            # alias gate keys on that difference).
            model_name=f"{name}-client",
            tenant_refs=[
                env["tenant_a"]["tenant_label_name"],
                env["tenant_b"]["tenant_label_name"],
            ],
        )
        _wait_attachments_ready(env, model)
        return {"model": model}

    @pytest.fixture(scope="class")
    def shared_serving(self, shared_env, shared_model):
        """Per-tenant auth/subscription stacks and API keys for the shared model."""
        env = shared_env
        model = shared_model["model"]
        _require_maas_route(model)
        access_a = _serve_in_tenant(env, env["tenant_a"], model)
        access_b = _serve_in_tenant(env, env["tenant_b"], model)
        return {"access_a": access_a, "access_b": access_b}

    def test_one_httproute_per_attachment(self, shared_env, shared_model):
        """Each attachment has its own HTTPRoute in the model namespace."""
        env = shared_env
        model = shared_model["model"]
        routes_by_gateway = {}
        for case in (env["tenant_a"], env["tenant_b"]):
            # Scoped by model name: other classes' models share this
            # namespace, and the exact-two-UIDs assertion below must not
            # count their routes.
            routes = wait_for_attachment_httproute(
                model["namespace"], case["gateway_name"], model_name=model["name"]
            )
            assert routes, f"no HTTPRoute attached to {case['gateway_name']}"
            routes_by_gateway[case["gateway_name"]] = routes
        route_uids = {
            route["metadata"]["uid"]
            for routes in routes_by_gateway.values()
            for route in routes
        }
        assert len(route_uids) == 2, (
            f"expected two distinct HTTPRoutes (one per gateway), got uids {route_uids}"
        )

    def test_per_gateway_status(self, shared_env, shared_model):
        """status.gateways carries an independent ready result per gateway."""
        env = shared_env
        model = shared_model["model"]
        if gateway_status_entry(
            model["name"], model["namespace"], env["tenant_a"]["gateway_name"]
        ) is None:
            pytest.skip(
                "AGC reports aggregate model status only (no status.gateways entries); "
                "per-gateway readiness is covered by test_both_gateways_serve"
            )
        for case in (env["tenant_a"], env["tenant_b"]):
            wait_for_gateway_attachment_ready(model["name"], model["namespace"], case["gateway_name"])

    def test_both_gateways_serve(self, shared_env, shared_model, shared_serving):
        """Real inference succeeds through both gateways with the same provider
        binding and credential."""
        env = shared_env
        model = shared_model["model"]
        _require_client_name_alias(model, env["tenant_a"])
        for case, access in (
            (env["tenant_a"], shared_serving["access_a"]),
            (env["tenant_b"], shared_serving["access_b"]),
        ):
            response = infer_external_chat(
                case["gateway_url"], model["namespace"], model["client_name"], access["api_key"],
                path_tag=model["path_tag"],
            )
            assert_served_by_provider(response, model["path_tag"], credential=model["credential"])


class TestTenantAuthIsolation:
    """Each tenant's key serves only through its own gateway.

    Both tenants authorize the model, so a cross-tenant rejection proves
    key isolation rather than an absent grant on the rejecting tenant's
    gateway."""

    @pytest.fixture(scope="class")
    def isolated_keys(self, shared_env):
        env = shared_env
        name = f"e2e-shr-auth-{uuid.uuid4().hex[:6]}"
        model = provision_external_model_stack(
            env["resources"],
            name=name,
            namespace=env["user_ns"],
            gateway_refs=[
                gateway_ref(env["tenant_a"]["gateway_name"]),
                gateway_ref(env["tenant_b"]["gateway_name"]),
            ],
            model_name=f"{name}-client",
            # Both tenants authorize the model, so a cross-tenant rejection
            # proves key isolation rather than a missing grant on the
            # rejecting tenant.
            tenant_refs=[
                env["tenant_a"]["tenant_label_name"],
                env["tenant_b"]["tenant_label_name"],
            ],
        )
        _wait_attachments_ready(env, model)
        _require_maas_route(model)
        access_a = _serve_in_tenant(env, env["tenant_a"], model)
        access_b = _serve_in_tenant(env, env["tenant_b"], model)
        return {
            "model": model,
            "key_a": access_a["api_key"],
            "key_b": access_b["api_key"],
        }

    def test_tenant_keys_authorized_only_on_their_own_gateway(self, shared_env, isolated_keys):
        """Each tenant's key serves through its own gateway and is rejected
        by the other's.

        The own-gateway 200 is the positive control: the other tenant's
        stack is live and authorizing the model, so the cross-tenant 401/403
        is a key-isolation verdict, not a missing grant."""
        env = shared_env
        model = isolated_keys["model"]
        _require_client_name_alias(model, env["tenant_a"])
        for case, other, api_key in (
            (env["tenant_a"], env["tenant_b"], isolated_keys["key_a"]),
            (env["tenant_b"], env["tenant_a"], isolated_keys["key_b"]),
        ):
            served = infer_external_chat(
                case["gateway_url"],
                model["namespace"],
                model["client_name"],
                api_key,
                path_tag=model["path_tag"],
            )
            assert_served_by_provider(
                served, model["path_tag"], credential=model["credential"]
            )
            rejected = infer_external_chat(
                other["gateway_url"],
                model["namespace"],
                model["client_name"],
                api_key,
                path_tag=model["path_tag"],
                retries=1,
            )
            assert rejected.status_code in (401, 403), (
                f"{case['tenant_label_name']}'s API key must be rejected by "
                f"{other['tenant_label_name']}'s gateway, got "
                f"{rejected.status_code}: {redact_sensitive(rejected.text)}"
            )

    def test_unknown_key_rejected_on_both_gateways(self, shared_env, isolated_keys):
        env = shared_env
        model = isolated_keys["model"]
        for case in (env["tenant_a"], env["tenant_b"]):
            response = infer_external_chat(
                case["gateway_url"],
                model["namespace"],
                model["client_name"],
                api_key=f"sk-extmod-invalid-{uuid.uuid4().hex[:8]}",
                path_tag=model["path_tag"],
                retries=1,
            )
            assert response.status_code in (401, 403), (
                f"unknown API key must be rejected by {case['gateway_name']}, got "
                f"{response.status_code}: {redact_sensitive(response.text)}"
            )


def _usage_provider(
    resources: TenantExternalModelResources, namespace: str, client_model_name: str
) -> dict:
    """Deploy the usage-returning provider the quota model binds to.

    A dedicated in-cluster ``llm-d-inference-sim`` (``--mode random``): its
    responses carry ``usage.total_tokens``, which token budgets are
    accounted from. The gateway envoy verifies provider certificates, so
    the simulator cannot serve a self-signed cert: its Service is annotated
    for an OpenShift service serving certificate (signed by the cluster
    service CA) and the provider points envoy at the CA bundle mounted in
    every pod through ``spec.config["tls.caCertificates"]``. The simulator
    validates the request body's ``model`` field, so it is configured with
    the client-facing model name the requests will carry.

    The simulator is deployed in the model's namespace and is dedicated to
    that model, so no rate limits apply on the provider hop: a 429 seen
    through a tenant gateway can only be that tenant's own budget.

    An echo endpoint cannot stand in even with a synthetic usage field: the
    telemetry pipeline drops reports whose body does not name the model and
    cannot read gzip-compressed responses (which httpbingo serves) — the
    simulator's plain OpenAI-shaped bodies satisfy all of it.
    """
    suffix = uuid.uuid4().hex[:6]
    sim = f"e2e-usage-sim-{suffix}"
    cert_secret = f"{sim}-cert"
    dns_name = f"{sim}.{namespace}.svc.cluster.local"
    _apply_cr(
        {
            "apiVersion": "v1",
            "kind": "Service",
            "metadata": {
                "name": sim,
                "namespace": namespace,
                "annotations": {SERVING_CERT_ANNOTATION: cert_secret},
            },
            "spec": {
                "selector": {"app": sim},
                # The ExternalProvider CRD rejects endpoints carrying a port
                # and the AGC dials 443, so the Service listens on 443 and
                # maps to the simulator's container port.
                "ports": [{"port": 443, "targetPort": 8000, "name": "https"}],
            },
        }
    )
    resources.track("service", sim, namespace)

    def _cert_injected() -> bool:
        secret = get_json_or_none("secret", cert_secret, namespace) or {}
        return bool((secret.get("data") or {}).get("tls.crt"))

    wait_until(_cert_injected, 120, f"service-ca cert secret {namespace}/{cert_secret}")

    _apply_cr(
        {
            "apiVersion": "apps/v1",
            "kind": "Deployment",
            "metadata": {"name": sim, "namespace": namespace},
            "spec": {
                "replicas": 1,
                "selector": {"matchLabels": {"app": sim}},
                "template": {
                    "metadata": {"labels": {"app": sim}},
                    "spec": {
                        "containers": [
                            {
                                "name": "sim",
                                "image": LLM_D_INFERENCE_SIM_IMAGE,
                                "command": ["/app/llm-d-inference-sim"],
                                "args": [
                                    "--port", "8000",
                                    "--model", client_model_name,
                                    "--mode", "random",
                                    "--no-mm-encoder-only",
                                    "--ssl-certfile", "/etc/sim/tls/tls.crt",
                                    "--ssl-keyfile", "/etc/sim/tls/tls.key",
                                ],
                                "ports": [
                                    {"containerPort": 8000, "name": "https", "protocol": "TCP"}
                                ],
                                "readinessProbe": {
                                    "httpGet": {
                                        "path": "/ready",
                                        "port": "https",
                                        "scheme": "HTTPS",
                                    }
                                },
                                "volumeMounts": [
                                    {"name": "tls", "mountPath": "/etc/sim/tls", "readOnly": True}
                                ],
                            }
                        ],
                        "volumes": [{"name": "tls", "secret": {"secretName": cert_secret}}],
                    },
                },
            },
        }
    )
    resources.track("deployment", sim, namespace)

    def _ready() -> bool:
        deployment = get_json_or_none("deployment", sim, namespace) or {}
        status = deployment.get("status") or {}
        return (status.get("availableReplicas") or 0) >= 1

    wait_until(_ready, 240, f"usage simulator deployment {namespace}/{sim}")
    return {
        "endpoint": dns_name,
        "provider_path": "/v1/chat/completions",
        "credential": f"sk-usage-{suffix}",
        "provider_config": {"tls.caCertificates": SA_SERVICE_CA},
    }


class TestTenantQuotaIsolation:
    """Quota consumption is tracked per tenant: exhausting tenant A's token
    budget leaves tenant B's untouched.

    Token budgets are accounted from ``usage.total_tokens`` in inference
    responses, so this class's model is served by a dedicated in-cluster
    simulator (see ``_usage_provider``) rather than the header-echo
    endpoint, whose responses carry no usage and could never exhaust a
    budget.
    """

    @pytest.fixture(scope="class")
    def quota_model(self, shared_env):
        env = shared_env
        name = f"e2e-shr-quota-{uuid.uuid4().hex[:6]}"
        client_name = f"{name}-client"
        usage = _usage_provider(env["resources"], env["user_ns"], client_name)
        model = provision_external_model_stack(
            env["resources"],
            name=name,
            namespace=env["user_ns"],
            gateway_refs=[
                gateway_ref(env["tenant_a"]["gateway_name"]),
                gateway_ref(env["tenant_b"]["gateway_name"]),
            ],
            model_name=client_name,
            tenant_refs=[
                env["tenant_a"]["tenant_label_name"],
                env["tenant_b"]["tenant_label_name"],
            ],
            endpoint=usage["endpoint"],
            provider_path=usage["provider_path"],
            credential=usage["credential"],
            provider_config=usage["provider_config"],
        )
        _wait_attachments_ready(env, model)
        _require_maas_route(model)
        access_a = _serve_in_tenant(
            env, env["tenant_a"], model, token_limit=LIMITED_TOKEN_LIMIT, window="1m"
        )
        access_b = _serve_in_tenant(
            env, env["tenant_b"], model, token_limit=UNLIMITED_TOKEN_LIMIT, window="1m"
        )
        return {"model": model, "access_a": access_a, "access_b": access_b, "usage": usage}

    def test_quota_exhaustion_isolated_per_tenant(self, shared_env, quota_model):
        env = shared_env
        model = quota_model["model"]
        provider_path = quota_model["usage"]["provider_path"]
        _require_client_name_alias(model, env["tenant_a"])

        # Drive tenant A past its budget: at least one 200 must precede the
        # 429 (a fresh budget always admits the first request — see
        # test_tenant_rate_limit_isolation._exhaust_until_429).
        successes = 0
        limited = None
        deadline = time.time() + 90
        while time.time() < deadline:
            limited = infer_external_chat(
                env["tenant_a"]["gateway_url"],
                model["namespace"],
                model["client_name"],
                quota_model["access_a"]["api_key"],
                provider_path=provider_path,
            )
            if limited.status_code == 200:
                successes += 1
            elif limited.status_code == 429:
                if successes:
                    break
            else:
                raise AssertionError(
                    f"unexpected inference response on tenant A: {redact_sensitive(limited.text)}"
                )
            time.sleep(1.0)
        assert limited is not None and limited.status_code == 429, (
            f"tenant A did not hit its token budget after {successes} successes: "
            f"{redact_sensitive(limited.text if limited is not None else 'no response')}"
        )
        assert successes > 0, "tenant A hit 429 before any successful inference (limits not live?)"

        # Tenant B's budget is independent and still serves — with usage,
        # which is what keeps its own budget accountable.
        response = infer_external_chat(
            env["tenant_b"]["gateway_url"],
            model["namespace"],
            model["client_name"],
            quota_model["access_b"]["api_key"],
            provider_path=provider_path,
        )
        assert_returns_usage(response)

        # And tenant A stays limited while tenant B keeps serving.
        still_limited = infer_external_chat(
            env["tenant_a"]["gateway_url"],
            model["namespace"],
            model["client_name"],
            quota_model["access_a"]["api_key"],
            provider_path=provider_path,
            retries=1,
        )
        assert still_limited.status_code == 429, (
            f"tenant A should remain rate limited, got {still_limited.status_code}: "
            f"{redact_sensitive(still_limited.text)}"
        )
        again_b = infer_external_chat(
            env["tenant_b"]["gateway_url"],
            model["namespace"],
            model["client_name"],
            quota_model["access_b"]["api_key"],
            provider_path=provider_path,
        )
        assert_returns_usage(again_b)
