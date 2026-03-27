"""
E2E tests for external model support.

Tests ExternalModel CRD + MaaSModelRef with external providers routed through
BBR payload processing plugins to an external simulator.

Prerequisites:
- MaaS deployed with ExternalModel reconciler (PR #582)
- BBR deployed with provider-resolver, api-translation, apikey-injection plugins
- External simulator accessible at E2E_SIMULATOR_ENDPOINT (default: 3.150.113.9)
- ServiceEntry for the simulator registered in the mesh

Environment variables:
- E2E_SIMULATOR_ENDPOINT: Simulator IP/FQDN (default: 3.150.113.9)
- E2E_EXTERNAL_SUBSCRIPTION: Subscription name for external models (default: e2e-external-subscription)
- GATEWAY_HOST: MaaS gateway hostname (required)
"""

import json
import logging
import os
import subprocess
import time
from typing import Optional

import pytest
import requests

log = logging.getLogger(__name__)

# ─── Configuration ──────────────────────────────────────────────────────────

SIMULATOR_ENDPOINT = os.environ.get("E2E_SIMULATOR_ENDPOINT", "3.150.113.9")
MODEL_NAMESPACE = os.environ.get("E2E_MODEL_NAMESPACE", "llm")
SUBSCRIPTION_NAMESPACE = os.environ.get("E2E_SUBSCRIPTION_NAMESPACE", "models-as-a-service")
EXTERNAL_SUBSCRIPTION = os.environ.get("E2E_EXTERNAL_SUBSCRIPTION", "e2e-external-subscription")
EXTERNAL_AUTH_POLICY = os.environ.get("E2E_EXTERNAL_AUTH_POLICY", "e2e-external-access")
RECONCILE_WAIT = int(os.environ.get("E2E_RECONCILE_WAIT", "12"))
TLS_VERIFY = os.environ.get("E2E_SKIP_TLS_VERIFY", "").lower() != "true"

PROVIDERS = [
    {"name": "e2e-openai",    "provider": "openai",        "key_value": "sk-openai-e2e-test"},
    {"name": "e2e-anthropic", "provider": "anthropic",      "key_value": "sk-ant-e2e-test"},
    {"name": "e2e-azure",     "provider": "azure-openai",   "key_value": "az-openai-e2e-test"},
    {"name": "e2e-vertex",    "provider": "vertex",         "key_value": "vtx-e2e-test"},
    {"name": "e2e-bedrock",   "provider": "bedrock-openai", "key_value": "bedrock-e2e-test"},
]


# ─── Helpers ─────────────────────────────────────────────────────────────────

def _apply_cr(cr_dict: dict):
    """Apply a Kubernetes CR from a dict."""
    result = subprocess.run(
        ["oc", "apply", "-f", "-"],
        input=json.dumps(cr_dict),
        capture_output=True, text=True,
    )
    if result.returncode != 0:
        log.warning(f"oc apply failed: {result.stderr}")
    return result.returncode == 0


def _delete_cr(kind: str, name: str, namespace: str):
    """Delete a Kubernetes resource (best effort)."""
    subprocess.run(
        ["oc", "delete", kind, name, "-n", namespace, "--ignore-not-found", "--timeout=30s"],
        capture_output=True, text=True,
    )


def _patch_cr(kind: str, name: str, namespace: str, patch: dict):
    """Patch a Kubernetes resource."""
    subprocess.run(
        ["oc", "patch", kind, name, "-n", namespace, "--type=merge", "-p", json.dumps(patch)],
        capture_output=True, text=True,
    )


def _get_cr(kind: str, name: str, namespace: str) -> Optional[dict]:
    """Get a Kubernetes resource as dict, or None if not found."""
    result = subprocess.run(
        ["oc", "get", kind, name, "-n", namespace, "-o", "json"],
        capture_output=True, text=True,
    )
    if result.returncode != 0:
        return None
    return json.loads(result.stdout)


def _wait_for_phase(kind: str, name: str, namespace: str, phase: str, timeout: int = 60) -> bool:
    """Wait for a CR to reach a specific status phase."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        cr = _get_cr(kind, name, namespace)
        if cr and cr.get("status", {}).get("phase") == phase:
            return True
        time.sleep(2)
    return False


# ─── Fixture: Create all external model resources ───────────────────────────

@pytest.fixture(scope="module")
def external_models_setup(gateway_url, headers, api_keys_base_url):
    """
    Create ExternalModel CRs, MaaSModelRefs, Secrets, AuthPolicy, and
    Subscription for all 5 providers. Cleanup after all tests in this module.
    """
    log.info("Setting up external model test fixtures for all providers...")

    # Create secrets
    for p in PROVIDERS:
        _apply_cr({
            "apiVersion": "v1",
            "kind": "Secret",
            "metadata": {
                "name": f"{p['name']}-api-key",
                "namespace": MODEL_NAMESPACE,
                "labels": {"inference.networking.k8s.io/bbr-managed": "true"},
            },
            "type": "Opaque",
            "stringData": {"api-key": p["key_value"]},
        })

    # Create ExternalModel CRs
    for p in PROVIDERS:
        _apply_cr({
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "ExternalModel",
            "metadata": {"name": p["name"], "namespace": MODEL_NAMESPACE},
            "spec": {
                "provider": p["provider"],
                "endpoint": SIMULATOR_ENDPOINT,
                "credentialRef": {
                    "name": f"{p['name']}-api-key",
                    "namespace": MODEL_NAMESPACE,
                },
            },
        })

    # Create MaaSModelRefs
    for p in PROVIDERS:
        _apply_cr({
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSModelRef",
            "metadata": {
                "name": p["name"],
                "namespace": MODEL_NAMESPACE,
                "annotations": {
                    "maas.opendatahub.io/endpoint": SIMULATOR_ENDPOINT,
                    "maas.opendatahub.io/provider": p["provider"],
                },
            },
            "spec": {
                "modelRef": {"kind": "ExternalModel", "name": p["name"]},
            },
        })

    # Create MaaSAuthPolicy
    _apply_cr({
        "apiVersion": "maas.opendatahub.io/v1alpha1",
        "kind": "MaaSAuthPolicy",
        "metadata": {"name": EXTERNAL_AUTH_POLICY, "namespace": SUBSCRIPTION_NAMESPACE},
        "spec": {
            "modelRefs": [{"name": p["name"], "namespace": MODEL_NAMESPACE} for p in PROVIDERS],
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
                {"name": p["name"], "namespace": MODEL_NAMESPACE,
                 "tokenRateLimits": [{"limit": 30, "window": "1m"}]}
                for p in PROVIDERS
            ],
        },
    })

    # Patch DestinationRules for simulator self-signed cert
    time.sleep(RECONCILE_WAIT)
    result = subprocess.run(
        ["oc", "get", "destinationrule", "-n", "openshift-ingress", "-o",
         "jsonpath={.items[*].metadata.name}"],
        capture_output=True, text=True,
    )
    for dr_name in result.stdout.split():
        if SIMULATOR_ENDPOINT.replace(".", "-") in dr_name:
            _patch_cr("destinationrule", dr_name, "openshift-ingress", {
                "spec": {"trafficPolicy": {"tls": {"mode": "SIMPLE", "insecureSkipVerify": True}}},
            })

    # Wait for auth to propagate
    time.sleep(RECONCILE_WAIT)

    # Create API key for inference tests
    log.info("Creating API key for external model inference tests...")
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
        "providers": PROVIDERS,
        "gateway_url": gateway_url,
    }

    # ── Cleanup ──
    log.info("Cleaning up external model test fixtures...")
    _delete_cr("maasauthpolicy", EXTERNAL_AUTH_POLICY, SUBSCRIPTION_NAMESPACE)
    _delete_cr("maassubscription", EXTERNAL_SUBSCRIPTION, SUBSCRIPTION_NAMESPACE)
    for p in PROVIDERS:
        # Remove finalizers first to avoid stuck deletion
        _patch_cr("maasmodelref", p["name"], MODEL_NAMESPACE,
                  {"metadata": {"finalizers": []}})
        _delete_cr("maasmodelref", p["name"], MODEL_NAMESPACE)
        _delete_cr("externalmodel", p["name"], MODEL_NAMESPACE)
        _delete_cr("secret", f"{p['name']}-api-key", MODEL_NAMESPACE)


# ─── Tests: Discovery ───────────────────────────────────────────────────────

class TestExternalModelDiscovery:
    """Verify external models are created and reconciled correctly."""

    @pytest.mark.parametrize("provider", PROVIDERS, ids=lambda p: p["provider"])
    def test_maasmodelref_created(self, external_models_setup, provider):
        """MaaSModelRef exists for each provider."""
        cr = _get_cr("maasmodelref", provider["name"], MODEL_NAMESPACE)
        assert cr is not None, f"MaaSModelRef {provider['name']} not found"

    @pytest.mark.parametrize("provider", PROVIDERS, ids=lambda p: p["provider"])
    def test_reconciler_created_httproute(self, external_models_setup, provider):
        """Reconciler created maas-model-* HTTPRoute in model namespace."""
        cr = _get_cr("httproute", f"maas-model-{provider['name']}", MODEL_NAMESPACE)
        assert cr is not None, f"HTTPRoute maas-model-{provider['name']} not found in {MODEL_NAMESPACE}"

    @pytest.mark.parametrize("provider", PROVIDERS, ids=lambda p: p["provider"])
    def test_reconciler_created_backend_service(self, external_models_setup, provider):
        """Reconciler created backend service in model namespace."""
        cr = _get_cr("service", f"maas-model-{provider['name']}-backend", MODEL_NAMESPACE)
        assert cr is not None, f"Service maas-model-{provider['name']}-backend not found"


# ─── Tests: Inference ────────────────────────────────────────────────────────

class TestExternalModelInference:
    """Verify inference works for all providers via the simulator."""

    @pytest.mark.parametrize("provider", PROVIDERS, ids=lambda p: p["provider"])
    def test_chat_completions_200(self, external_models_setup, provider):
        """POST /<model>/v1/chat/completions returns 200."""
        setup = external_models_setup
        url = f"{setup['gateway_url']}/{provider['name']}/v1/chat/completions"
        headers = {
            "Content-Type": "application/json",
            "Authorization": f"Bearer {setup['api_key']}",
        }
        body = {
            "model": provider["name"],
            "messages": [{"role": "user", "content": f"hello from {provider['provider']}"}],
        }

        r = requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)
        assert r.status_code == 200, (
            f"Expected 200 for {provider['provider']}, got {r.status_code}: {r.text[:200]}"
        )

    @pytest.mark.parametrize("provider", PROVIDERS, ids=lambda p: p["provider"])
    def test_response_is_valid_openai_format(self, external_models_setup, provider):
        """Response contains standard OpenAI chat completion fields."""
        setup = external_models_setup
        url = f"{setup['gateway_url']}/{provider['name']}/v1/chat/completions"
        headers = {
            "Content-Type": "application/json",
            "Authorization": f"Bearer {setup['api_key']}",
        }
        body = {
            "model": provider["name"],
            "messages": [{"role": "user", "content": "hello"}],
        }

        r = requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)
        assert r.status_code == 200

        data = r.json()
        assert "choices" in data, f"Response missing 'choices': {data}"
        assert "model" in data, f"Response missing 'model': {data}"
        assert len(data["choices"]) > 0, f"Empty choices: {data}"
        assert "message" in data["choices"][0], f"Choice missing 'message': {data}"


# ─── Tests: Auth ─────────────────────────────────────────────────────────────

class TestExternalModelAuth:
    """Verify auth enforcement for external models."""

    def test_invalid_key_returns_401(self, external_models_setup):
        """Invalid API key returns 401."""
        setup = external_models_setup
        provider = PROVIDERS[0]  # test with first provider
        url = f"{setup['gateway_url']}/{provider['name']}/v1/chat/completions"
        headers = {
            "Content-Type": "application/json",
            "Authorization": "Bearer INVALID-KEY-12345",
        }
        body = {"model": provider["name"], "messages": [{"role": "user", "content": "hello"}]}

        r = requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)
        assert r.status_code in (401, 403), f"Expected 401/403, got {r.status_code}"

    def test_no_key_returns_401(self, external_models_setup):
        """No API key returns 401."""
        setup = external_models_setup
        provider = PROVIDERS[0]
        url = f"{setup['gateway_url']}/{provider['name']}/v1/chat/completions"
        headers = {"Content-Type": "application/json"}
        body = {"model": provider["name"], "messages": [{"role": "user", "content": "hello"}]}

        r = requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)
        assert r.status_code in (401, 403), f"Expected 401/403, got {r.status_code}"


# ─── Tests: Rate Limiting ───────────────────────────────────────────────────

class TestExternalModelRateLimiting:
    """Verify token rate limiting for external models."""

    def test_rate_limit_returns_429(self, external_models_setup):
        """
        Exceeding token rate limit returns 429.
        Subscription is set to 30 tokens/min. Each response uses ~10 tokens.
        After 3-4 requests, should get 429.
        """
        setup = external_models_setup
        provider = PROVIDERS[1]  # use anthropic (second provider)
        url = f"{setup['gateway_url']}/{provider['name']}/v1/chat/completions"
        headers = {
            "Content-Type": "application/json",
            "Authorization": f"Bearer {setup['api_key']}",
        }
        body = {"model": provider["name"], "messages": [{"role": "user", "content": "hello"}]}

        status_codes = []
        for i in range(6):
            r = requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)
            status_codes.append(r.status_code)
            log.info(f"Rate limit request {i+1}: HTTP {r.status_code}")

        assert 429 in status_codes, (
            f"Expected at least one 429 in {status_codes}. "
            f"Rate limit (30 tokens/min) should trigger after ~3 requests."
        )


# ─── Tests: Cleanup ─────────────────────────────────────────────────────────

class TestExternalModelCleanup:
    """Verify resource cleanup when external models are deleted."""

    def test_delete_removes_httproute(self, external_models_setup):
        """
        Deleting a MaaSModelRef should remove the maas-model-* HTTPRoute
        via the finalizer. Test with a temporary model.
        """
        temp_name = "e2e-cleanup-test"

        # Create temporary model
        _apply_cr({
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "ExternalModel",
            "metadata": {"name": temp_name, "namespace": MODEL_NAMESPACE},
            "spec": {
                "provider": "openai",
                "endpoint": SIMULATOR_ENDPOINT,
                "credentialRef": {"name": "e2e-openai-api-key", "namespace": MODEL_NAMESPACE},
            },
        })
        _apply_cr({
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSModelRef",
            "metadata": {
                "name": temp_name,
                "namespace": MODEL_NAMESPACE,
                "annotations": {
                    "maas.opendatahub.io/endpoint": SIMULATOR_ENDPOINT,
                    "maas.opendatahub.io/provider": "openai",
                },
            },
            "spec": {"modelRef": {"kind": "ExternalModel", "name": temp_name}},
        })

        # Wait for reconciler
        time.sleep(RECONCILE_WAIT)

        # Verify HTTPRoute was created
        route = _get_cr("httproute", f"maas-model-{temp_name}", MODEL_NAMESPACE)
        assert route is not None, f"HTTPRoute maas-model-{temp_name} should exist before deletion"

        # Delete
        _delete_cr("maasmodelref", temp_name, MODEL_NAMESPACE)
        time.sleep(RECONCILE_WAIT)

        # Verify HTTPRoute was cleaned up
        route = _get_cr("httproute", f"maas-model-{temp_name}", MODEL_NAMESPACE)
        assert route is None, f"HTTPRoute maas-model-{temp_name} should be cleaned up after deletion"

        # Cleanup ExternalModel
        _delete_cr("externalmodel", temp_name, MODEL_NAMESPACE)
