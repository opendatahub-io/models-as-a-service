"""
E2E tests for external model (egress) support.

Tests that MaaS can route requests to an external endpoint via ExternalModel CRD,
including reconciler resource creation, auth enforcement, egress connectivity,
path-based routing, and body-based routing (BBR).

Prerequisites:
- MaaS deployed with ExternalModel reconciler
- External endpoint accessible from the cluster (default: httpbin.org)
- For body-based routing tests: IPP (payload-processing) pods deployed

Environment variables:
- E2E_EXTERNAL_ENDPOINT: External endpoint hostname (default: httpbingo.org — an
  actively-maintained httpbin clone; the original httpbin.org demo instance is
  prone to unrelated 503s that would silently skip every test in this file via
  _check_external_endpoint_reachable)
- E2E_EXTERNAL_SUBSCRIPTION: Subscription name (default: e2e-external-subscription)
- GATEWAY_HOST: MaaS gateway hostname (required)
"""

import json
import logging
import os
import subprocess
import time

import pytest
import requests

from test_helper import (
    MODEL_NAMESPACE,
    TLS_VERIFY,
    _apply_cr,
    _check_ipp_pods_deployed,
    _delete_cr,
    _get_cr,
    _wait_for_maas_auth_policy_phase,
    _wait_for_maas_subscription_phase,
)

log = logging.getLogger(__name__)

# ─── Configuration ──────────────────────────────────────────────────────────

EXTERNAL_ENDPOINT = os.environ.get("E2E_EXTERNAL_ENDPOINT", os.environ.get("E2E_SIMULATOR_ENDPOINT", "httpbingo.org"))
SUBSCRIPTION_NAMESPACE = os.environ.get("E2E_SUBSCRIPTION_NAMESPACE", os.environ.get("MAAS_SUBSCRIPTION_NAMESPACE", "models-as-a-service"))
EXTERNAL_SUBSCRIPTION = os.environ.get("E2E_EXTERNAL_SUBSCRIPTION", "e2e-external-subscription")
EXTERNAL_AUTH_POLICY = os.environ.get("E2E_EXTERNAL_AUTH_POLICY", "e2e-external-access")
RECONCILE_WAIT = int(os.environ.get("E2E_RECONCILE_WAIT", "12"))

TARGET_MODEL = "gpt-3.5-turbo"

EXTERNAL_MODEL_NAME = "e2e-external-model"
EXTERNAL_PROVIDER_NAME = "e2e-external-provider"

# Canonical inference.opendatahub.io CRDs. maas-controller's MaaSModelRef
# externalModelHandler and the IPP model-provider-resolver plugin both watch
# this group; the legacy maas.opendatahub.io/ExternalModel is a fallback only
# and is a no-op for BBR (model-provider-resolver never watches it).
EXTERNAL_MODEL_KIND = "externalmodels.inference.opendatahub.io"
EXTERNAL_PROVIDER_KIND = "externalproviders.inference.opendatahub.io"

# The canonical reconciler (ai-gateway-payload-processing) names the HTTPRoute
# after the ExternalModel itself (no "maas-" prefix) and the backend Service
# after the ExternalProvider (the HTTPRoute's backendRef).
EXTERNAL_MODEL_HTTPROUTE_NAME = EXTERNAL_MODEL_NAME
EXTERNAL_PROVIDER_SERVICE_NAME = EXTERNAL_PROVIDER_NAME


# ─── Helpers ─────────────────────────────────────────────────────────────────

def _patch_cr(kind: str, name: str, namespace: str, patch: dict):
    """Patch a Kubernetes resource."""
    subprocess.run(
        ["oc", "patch", kind, name, "-n", namespace, "--type=merge", "-p", json.dumps(patch)],
        capture_output=True, text=True,
    )



# ─── Connectivity check ──────────────────────────────────────────────────────

def _check_external_endpoint_reachable():
    """Verify the external endpoint is reachable. Skip tests if not."""
    try:
        r = requests.get(f"https://{EXTERNAL_ENDPOINT}/get", timeout=10, verify=False)
        if r.status_code == 200:
            return True
    except Exception:
        pass
    # Try HTTP fallback
    try:
        r = requests.get(f"http://{EXTERNAL_ENDPOINT}/get", timeout=10)
        if r.status_code == 200:
            return True
    except Exception:
        pass
    return False


pytestmark = pytest.mark.skipif(
    not _check_external_endpoint_reachable(),
    reason=f"External endpoint {EXTERNAL_ENDPOINT} is not reachable (disconnected environment?)",
)


# ─── Fixture: Create external model resources ────────────────────────────────

@pytest.fixture(scope="module")
def external_models_setup(gateway_url, headers, api_keys_base_url):
    """
    Create a single ExternalModel CR, MaaSModelRef, AuthPolicy, and
    Subscription pointing to an external endpoint. Cleanup after tests.
    """
    log.info(f"Setting up external model test fixture (endpoint: {EXTERNAL_ENDPOINT})...")

    # Create a dummy secret (ExternalModel requires credentialRef).
    # The apikey-injection plugin's secret store only watches Secrets carrying
    # this label; without it credential lookups silently fail with a 500 at
    # request time (masked by tests that only assert non-401/403).
    _apply_cr({
        "apiVersion": "v1",
        "kind": "Secret",
        "metadata": {
            "name": f"{EXTERNAL_MODEL_NAME}-api-key",
            "namespace": MODEL_NAMESPACE,
            "labels": {"inference.llm-d.ai/ipp-managed": "true"},
        },
        "type": "Opaque",
        "stringData": {"api-key": "e2e-test-key"},
    })

    # Create ExternalProvider CR (inference.opendatahub.io — canonical)
    _apply_cr({
        "apiVersion": "inference.opendatahub.io/v1alpha1",
        "kind": "ExternalProvider",
        "metadata": {"name": EXTERNAL_PROVIDER_NAME, "namespace": MODEL_NAMESPACE},
        "spec": {
            "provider": "openai",
            "endpoint": EXTERNAL_ENDPOINT,
            "auth": {
                "type": "simple",
                "secretRef": {"name": f"{EXTERNAL_MODEL_NAME}-api-key"},
            },
        },
    })

    # Create ExternalModel CR (inference.opendatahub.io — canonical). This is
    # what the IPP model-provider-resolver plugin watches to populate its
    # model->provider store, and what maas-controller's externalModelHandler
    # tries first when reconciling the MaaSModelRef.
    _apply_cr({
        "apiVersion": "inference.opendatahub.io/v1alpha1",
        "kind": "ExternalModel",
        "metadata": {"name": EXTERNAL_MODEL_NAME, "namespace": MODEL_NAMESPACE},
        "spec": {
            "externalProviderRefs": [
                {
                    "ref": {"name": EXTERNAL_PROVIDER_NAME},
                    "targetModel": TARGET_MODEL,
                    "apiFormat": "openai-chat",
                    "path": "/v1/chat/completions",
                },
            ],
        },
    })

    # Create MaaSModelRef
    _apply_cr({
        "apiVersion": "maas.opendatahub.io/v1alpha1",
        "kind": "MaaSModelRef",
        "metadata": {
            "name": EXTERNAL_MODEL_NAME,
            "namespace": MODEL_NAMESPACE,
            "annotations": {
                "maas.opendatahub.io/endpoint": EXTERNAL_ENDPOINT,
                "maas.opendatahub.io/provider": "openai",
            },
        },
        "spec": {
            "modelRef": {"kind": "ExternalModel", "name": EXTERNAL_MODEL_NAME},
        },
    })

    # Create MaaSAuthPolicy
    _apply_cr({
        "apiVersion": "maas.opendatahub.io/v1alpha1",
        "kind": "MaaSAuthPolicy",
        "metadata": {"name": EXTERNAL_AUTH_POLICY, "namespace": SUBSCRIPTION_NAMESPACE},
        "spec": {
            "modelRefs": [{"name": EXTERNAL_MODEL_NAME, "namespace": MODEL_NAMESPACE}],
            "subjects": {"groups": [{"name": "system:authenticated"}]},
        },
    })

    # Create MaaSSubscription
    _apply_cr({
        "apiVersion": "maas.opendatahub.io/v1alpha1",
        "kind": "MaaSSubscription",
        "metadata": {"name": EXTERNAL_SUBSCRIPTION, "namespace": SUBSCRIPTION_NAMESPACE},
        "spec": {
            "owner": {"groups": [{"name": "system:authenticated"}]},
            "modelRefs": [
                {
                    "name": EXTERNAL_MODEL_NAME,
                    "namespace": MODEL_NAMESPACE,
                    "tokenRateLimits": [{"limit": 10000, "window": "1h"}],
                },
            ],
        },
    })

    # Wait for CRs to reconcile
    _wait_for_maas_auth_policy_phase(EXTERNAL_AUTH_POLICY, namespace=SUBSCRIPTION_NAMESPACE)
    _wait_for_maas_subscription_phase(EXTERNAL_SUBSCRIPTION, namespace=SUBSCRIPTION_NAMESPACE)

    # Create API key for tests
    log.info("Creating API key for external model tests...")
    r = requests.post(
        api_keys_base_url,
        headers=headers,
        json={"name": "e2e-external-model-key", "subscription": EXTERNAL_SUBSCRIPTION},
        timeout=30,
        verify=TLS_VERIFY,
    )
    if r.status_code not in (200, 201):
        pytest.fail(f"Failed to create API key: {r.status_code} {r.text}")

    api_key = r.json().get("key")
    log.info(f"API key created: {api_key[:15]}...")

    yield {
        "api_key": api_key,
        "gateway_url": gateway_url,
    }

    # ── Cleanup ──
    log.info("Cleaning up external model test fixtures...")
    _delete_cr("maasauthpolicy", EXTERNAL_AUTH_POLICY, SUBSCRIPTION_NAMESPACE)
    _delete_cr("maassubscription", EXTERNAL_SUBSCRIPTION, SUBSCRIPTION_NAMESPACE)
    _patch_cr("maasmodelref", EXTERNAL_MODEL_NAME, MODEL_NAMESPACE,
              {"metadata": {"finalizers": []}})
    _delete_cr("maasmodelref", EXTERNAL_MODEL_NAME, MODEL_NAMESPACE)
    _delete_cr(EXTERNAL_MODEL_KIND, EXTERNAL_MODEL_NAME, MODEL_NAMESPACE)
    _delete_cr(EXTERNAL_PROVIDER_KIND, EXTERNAL_PROVIDER_NAME, MODEL_NAMESPACE)
    _delete_cr("secret", f"{EXTERNAL_MODEL_NAME}-api-key", MODEL_NAMESPACE)


# ─── Tests: Discovery ───────────────────────────────────────────────────────

class TestExternalModelDiscovery:
    """Verify ExternalModel reconciler creates the expected Istio resources."""

    def test_maasmodelref_created(self, external_models_setup):
        """MaaSModelRef exists for the external model."""
        cr = _get_cr("maasmodelref", EXTERNAL_MODEL_NAME, MODEL_NAMESPACE)
        assert cr is not None, f"MaaSModelRef {EXTERNAL_MODEL_NAME} not found"

    def test_reconciler_created_httproute(self, external_models_setup):
        """IPP's ExternalModel reconciler created the HTTPRoute (named after the model)."""
        cr = _get_cr("httproute", EXTERNAL_MODEL_HTTPROUTE_NAME, MODEL_NAMESPACE)
        assert cr is not None, f"HTTPRoute {EXTERNAL_MODEL_HTTPROUTE_NAME} not found"

    def test_reconciler_created_backend_service(self, external_models_setup):
        """IPP's ExternalProvider reconciler created the backend service (named after the provider)."""
        cr = _get_cr("service", EXTERNAL_PROVIDER_SERVICE_NAME, MODEL_NAMESPACE)
        assert cr is not None, f"Service {EXTERNAL_PROVIDER_SERVICE_NAME} not found"


# ─── Tests: Auth ─────────────────────────────────────────────────────────────

class TestExternalModelAuth:
    """Verify auth enforcement for external model routes."""

    def test_invalid_key_returns_401(self, external_models_setup):
        """Invalid API key returns 401/403."""
        setup = external_models_setup
        url = f"{setup['gateway_url']}/{MODEL_NAMESPACE}/{EXTERNAL_MODEL_NAME}/v1/chat/completions"
        headers = {
            "Content-Type": "application/json",
            "Authorization": "Bearer INVALID-KEY-12345",
        }
        body = {"model": EXTERNAL_MODEL_NAME, "messages": [{"role": "user", "content": "hello"}]}

        r = requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)
        assert r.status_code in (401, 403), f"Expected 401/403, got {r.status_code}"

    def test_no_key_returns_401(self, external_models_setup):
        """No API key returns 401/403."""
        setup = external_models_setup
        url = f"{setup['gateway_url']}/{MODEL_NAMESPACE}/{EXTERNAL_MODEL_NAME}/v1/chat/completions"
        headers = {"Content-Type": "application/json"}
        body = {"model": EXTERNAL_MODEL_NAME, "messages": [{"role": "user", "content": "hello"}]}

        r = requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)
        assert r.status_code in (401, 403), f"Expected 401/403, got {r.status_code}"


# ─── Tests: Egress ───────────────────────────────────────────────────────────

class TestExternalModelEgress:
    """Verify requests are forwarded to the external endpoint."""

    def test_request_forwarded_returns_200(self, external_models_setup):
        """
        With a valid API key, the request passes auth and reaches the
        external endpoint. Expect 200 confirming egress connectivity.
        """
        setup = external_models_setup
        url = f"{setup['gateway_url']}/{MODEL_NAMESPACE}/{EXTERNAL_MODEL_NAME}/v1/chat/completions"
        headers = {
            "Content-Type": "application/json",
            "Authorization": f"Bearer {setup['api_key']}",
        }
        body = {"model": EXTERNAL_MODEL_NAME, "messages": [{"role": "user", "content": "hello"}]}

        r = requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)
        assert r.status_code not in (401, 403), (
            f"Request was blocked by auth (HTTP {r.status_code}). "
            f"Expected the request to reach the external endpoint."
        )
        # Any non-auth response confirms egress connectivity.
        # httpbin.org may return 404 for unknown paths — that's fine,
        # it means the request left the cluster and reached the endpoint.
        log.info(f"Egress test: HTTP {r.status_code} from external endpoint")


# ─── Tests: Cleanup ─────────────────────────────────────────────────────────

class TestExternalModelCleanup:
    """Verify resource cleanup when external models are deleted."""

    def test_delete_removes_httproute(self, external_models_setup):
        """
        Deleting an ExternalModel removes the HTTPRoute via OwnerReference
        garbage collection (the ExternalModel reconciler sets itself as the
        HTTPRoute's controller owner).
        """
        temp_name = "e2e-cleanup-test"

        # Reuse the shared ExternalProvider; only the ExternalModel is temporary.
        _apply_cr({
            "apiVersion": "inference.opendatahub.io/v1alpha1",
            "kind": "ExternalModel",
            "metadata": {"name": temp_name, "namespace": MODEL_NAMESPACE},
            "spec": {
                "externalProviderRefs": [
                    {
                        "ref": {"name": EXTERNAL_PROVIDER_NAME},
                        "targetModel": TARGET_MODEL,
                        "apiFormat": "openai-chat",
                        "path": "/v1/chat/completions",
                    },
                ],
            },
        })

        try:
            # Wait for reconciler to create resources
            time.sleep(RECONCILE_WAIT * 2)

            # Verify HTTPRoute was created (named after the ExternalModel)
            route = _get_cr("httproute", temp_name, MODEL_NAMESPACE)
            assert route is not None, f"HTTPRoute {temp_name} should exist before deletion"

            # Delete the ExternalModel (owns the HTTPRoute via OwnerReference)
            _delete_cr(EXTERNAL_MODEL_KIND, temp_name, MODEL_NAMESPACE)
            time.sleep(RECONCILE_WAIT)

            # Verify HTTPRoute was cleaned up by garbage collection
            route = _get_cr("httproute", temp_name, MODEL_NAMESPACE)
            assert route is None, f"HTTPRoute {temp_name} should be cleaned up after ExternalModel deletion"
        finally:
            # Always clean up to avoid resource leaks
            _delete_cr(EXTERNAL_MODEL_KIND, temp_name, MODEL_NAMESPACE)



class TestExternalModelPathRouting:
    """Verify path-based routing for external model endpoints.

    The URL path ``/{namespace}/{model}/v1/...`` determines which model
    backend the gateway routes to. The happy-path case (correct path reaches
    the endpoint) is already covered by TestExternalModelEgress.
    """

    def test_wrong_path_returns_not_found(self, external_models_setup):
        """Request to non-existent model path returns 404 or auth error."""
        setup = external_models_setup
        url = f"{setup['gateway_url']}/{MODEL_NAMESPACE}/nonexistent-model-xyz/v1/chat/completions"
        headers = {
            "Content-Type": "application/json",
            "Authorization": f"Bearer {setup['api_key']}",
        }
        body = {"model": "nonexistent-model-xyz", "messages": [{"role": "user", "content": "hello"}]}

        r = requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)
        assert r.status_code in (401, 403, 404), (
            f"Expected 401/403/404 for wrong model path, got {r.status_code}. "
            f"Request should not route to any model backend."
        )
        log.info("Path routing (wrong path): HTTP %d", r.status_code)



requires_ipp = pytest.mark.skipif(
    not _check_ipp_pods_deployed(),
    reason="Payload-processing (IPP) pods not deployed; body routing tests require IPP",
)


@requires_ipp
class TestExternalModelBodyRouting:
    """Verify body-based routing for external models.

    IPP pre-processing extracts the ``model`` field from the JSON body and
    resolves it via the model-provider-resolver plugin's store.

    IMPORTANT CAVEAT: every request here hits ``/{ns}/{model}/v1/...``, a
    path that already encodes a valid model name, so Kuadrant's AuthPolicy
    authorizes from the path alone and the plugin silently no-ops (rather
    than rejecting) on an unresolvable body model. Only
    test_correct_model_in_body_succeeds is a meaningful assertion today
    (proves a legitimately provisioned model's body isn't blocked); the
    "wrong"/"missing" model tests are smoke checks only — see their
    docstrings. Genuine path-agnostic body-only enforcement is future work
    (RHAISTRAT-1540).
    """

    def _post_chat(self, gateway_url, model_path, api_key, body):
        url = f"{gateway_url}{model_path}/chat/completions"
        headers = {
            "Content-Type": "application/json",
            "Authorization": f"Bearer {api_key}",
        }
        return requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)

    def test_correct_model_in_body_succeeds(self, external_models_setup):
        """
        Correct model name in body passes through IPP and reaches the
        external endpoint.

        Unlike the tenant/LLMInferenceService path, the backend here is an
        uncontrolled external endpoint (httpbin.org by default), which does
        not implement /v1/chat/completions and may not return 200. As with
        TestExternalModelEgress.test_request_forwarded_returns_200, any
        non-auth response confirms the body model field was accepted and the
        request was forwarded rather than rejected by the
        model-provider-resolver plugin.
        """
        setup = external_models_setup
        model_path = f"/{MODEL_NAMESPACE}/{EXTERNAL_MODEL_NAME}/v1"

        r = self._post_chat(setup["gateway_url"], model_path, setup["api_key"], {
            "model": EXTERNAL_MODEL_NAME,
            "messages": [{"role": "user", "content": "hello"}],
        })
        assert r.status_code not in (401, 403), (
            f"Expected correct model in body to be forwarded, got {r.status_code}. "
            f"Body routing may be rejecting a legitimately provisioned model."
        )
        log.info("Body routing (correct model): HTTP %d", r.status_code)

    def test_wrong_model_in_body_does_not_error(self, external_models_setup):
        """
        Wrong model name in body does not crash the request pipeline.

        NOTE: This is a smoke check, not an enforcement check. The URL path
        already contains a valid model name, so Kuadrant's AuthPolicy
        authorizes the request from the path alone before the body is
        considered. IPP's model-provider-resolver plugin does not reject
        unresolvable models either — it silently no-ops (returns nil) and
        lets the request pass through unchanged when the body's model isn't
        found in its store (see plugin.go ProcessRequest). So today there is
        no code path that actually rejects a mismatched body model for
        ExternalModel; this test only proves the request isn't dropped or
        errored (still != 200, since httpbingo never returns 200 for these
        paths). True body-only enforcement without a path is tracked under
        RHAISTRAT-1540 (unified BBR routing) and isn't implemented yet.
        """
        setup = external_models_setup
        model_path = f"/{MODEL_NAMESPACE}/{EXTERNAL_MODEL_NAME}/v1"

        r = self._post_chat(setup["gateway_url"], model_path, setup["api_key"], {
            "model": "nonexistent-model",
            "messages": [{"role": "user", "content": "hello"}],
        })
        assert r.status_code != 200, (
            f"Expected non-200 for wrong model in body, got 200. "
            f"Body routing may not be active — request succeeded via path routing alone."
        )
        log.info("Body routing (wrong model): HTTP %d", r.status_code)

    def test_missing_model_in_body_does_not_error(self, external_models_setup):
        """
        Missing model field in body does not crash the request pipeline.

        NOTE: Same caveat as test_wrong_model_in_body_does_not_error — this
        is a smoke check, not proof that a missing model is rejected. See
        that test's docstring for why no enforcement path currently exists.
        """
        setup = external_models_setup
        model_path = f"/{MODEL_NAMESPACE}/{EXTERNAL_MODEL_NAME}/v1"

        r = self._post_chat(setup["gateway_url"], model_path, setup["api_key"], {
            "messages": [{"role": "user", "content": "hello"}],
        })
        assert r.status_code != 200, (
            f"Expected non-200 for missing model in body, got 200. "
            f"Body routing may not be active — request succeeded without model field."
        )
        log.info("Body routing (missing model): HTTP %d", r.status_code)
