"""
Embedding inference e2e tests.

Validates the /v1/embeddings endpoint across routing modes (path-based, BBR),
governance (TRLP rate limiting), and default-deny (no auth/subscription → 403).

The embedding simulator fixture (test/e2e/fixtures/embedding/) deploys a
dedicated LLMInferenceService with --no-mm-encoder-only to serve the full
OpenAI surface including /v1/embeddings.

Requires:
  - GATEWAY_HOST env var
  - MAAS_API_BASE_URL env var
  - maas-controller deployed with test/e2e/fixtures applied
  - oc/kubectl access
"""

import logging
import time
import uuid

import pytest
import requests

from test_helper import (
    EMBEDDING_MODEL_NAME,
    EMBEDDING_MODEL_PATH,
    EMBEDDING_MODEL_REF,
    EMBEDDING_MODEL_CANONICAL_ID,
    MODEL_NAMESPACE,
    RECONCILE_WAIT,
    TIMEOUT,
    TLS_VERIFY,
    _create_api_key,
    _create_test_auth_policy,
    _create_test_subscription,
    _delete_cr,
    _embedding_inference,
    _gateway_url,
    _get_cluster_token,
    _get_cr,
    _poll_status,
    _wait_for_maas_auth_policy_phase,
    _wait_for_maas_subscription_phase,
    _wait_for_token_rate_limit_policy,
    embeddings,
)

log = logging.getLogger(__name__)


class TestEmbeddingPathRouting:
    """Path-based and BBR embedding inference (read-only, uses existing fixtures)."""

    pytestmark = pytest.mark.xdist_group("api_keys")

    def test_embedding_path_based_200(self, model_v1: str, api_key_headers: dict, model_name: str):
        """POST /{ns}/{model}/v1/embeddings returns valid embedding response."""
        r = embeddings("The quick brown fox", model_v1, api_key_headers, model_name=model_name)
        log.info(f"[embedding] POST /v1/embeddings -> {r.status_code}")
        assert r.status_code == 200, f"Expected 200, got {r.status_code}: {r.text[:500]}"
        data = r.json()
        assert "data" in data, f"Missing 'data' in response: {list(data.keys())}"
        assert len(data["data"]) > 0, "Empty data array"
        first = data["data"][0]
        assert "embedding" in first, f"Missing 'embedding' in data[0]: {list(first.keys())}"
        assert isinstance(first["embedding"], list), "embedding should be a list of floats"
        assert len(first["embedding"]) > 0, "embedding vector is empty"
        usage = data.get("usage", {})
        assert usage.get("prompt_tokens", 0) > 0, f"Expected prompt_tokens > 0, got {usage}"

    def test_embedding_bbr_llmisvc_200(self, gateway_url: str, api_key_headers: dict):
        """POST /v1/embeddings with canonical model ID routes via BBR."""
        url = f"{gateway_url}/v1/embeddings"
        body = {"model": EMBEDDING_MODEL_CANONICAL_ID, "input": "The quick brown fox"}
        r = requests.post(url, headers=api_key_headers, json=body, timeout=30, verify=TLS_VERIFY)
        log.info(f"[embedding-bbr] POST /v1/embeddings (model={EMBEDDING_MODEL_CANONICAL_ID}) -> {r.status_code}")
        assert r.status_code == 200, f"Expected 200, got {r.status_code}: {r.text[:500]}"
        data = r.json()
        assert "data" in data, f"Missing 'data' in response: {list(data.keys())}"

    @pytest.mark.skip(reason="Depends on ExternalModel BBR gateway story")
    def test_embedding_bbr_external_model(self):
        """POST /v1/embeddings targeting an ExternalModel embedding backend."""
        pass


class TestEmbeddingGovernance:
    """Embedding TRLP enforcement and default-deny tests.

    Uses the dedicated e2e-embedding-simulated fixture for isolation.
    """

    pytestmark = [pytest.mark.serial, pytest.mark.xdist_group("api_keys")]

    def test_embedding_default_deny_403(self):
        """Embedding model with no auth policy or subscription gets 403."""
        model = _get_cr("maasmodelref", EMBEDDING_MODEL_REF, namespace=MODEL_NAMESPACE)
        if not model:
            pytest.skip(f"MaaSModelRef {EMBEDDING_MODEL_REF} not deployed")

        oc_token = _get_cluster_token()
        url = f"{_gateway_url()}{EMBEDDING_MODEL_PATH}/v1/embeddings"
        headers = {"Authorization": f"Bearer {oc_token}", "Content-Type": "application/json"}
        r = requests.post(
            url, headers=headers,
            json={"model": EMBEDDING_MODEL_NAME, "input": "Hello world"},
            timeout=TIMEOUT, verify=TLS_VERIFY,
        )
        log.info(f"[embedding-deny] No auth/subscription -> {r.status_code}")
        assert r.status_code == 403, f"Expected 403, got {r.status_code}: {r.text[:500]}"

    def test_embedding_trlp_429(self):
        """Embedding requests get 429 when token budget is exhausted."""
        model = _get_cr("maasmodelref", EMBEDDING_MODEL_REF, namespace=MODEL_NAMESPACE)
        if not model:
            pytest.skip(f"MaaSModelRef {EMBEDDING_MODEL_REF} not deployed")

        auth_policy_name = "e2e-embedding-trlp-auth"
        subscription_name = "e2e-embedding-trlp-sub"
        token_limit = 5
        window = "1m"
        total_requests = 15

        try:
            _create_test_auth_policy(
                name=auth_policy_name,
                model_refs=[EMBEDDING_MODEL_REF],
                groups=["system:authenticated"],
            )
            time.sleep(RECONCILE_WAIT)

            _create_test_subscription(
                name=subscription_name,
                model_refs=[EMBEDDING_MODEL_REF],
                groups=["system:authenticated"],
                token_limit=token_limit,
                window=window,
            )
            time.sleep(RECONCILE_WAIT)

            _wait_for_token_rate_limit_policy(
                EMBEDDING_MODEL_REF, model_namespace=MODEL_NAMESPACE, timeout=90
            )

            oc_token = _get_cluster_token()
            api_key = _create_api_key(
                oc_token,
                name=f"e2e-emb-trlp-{uuid.uuid4().hex[:8]}",
                subscription=subscription_name,
            )

            rate_limited = False
            success_count = 0

            for i in range(total_requests):
                r = _embedding_inference(
                    api_key, path=EMBEDDING_MODEL_PATH, model_name=EMBEDDING_MODEL_NAME
                )
                request_num = i + 1
                log.info(f"Embedding request {request_num}/{total_requests}: {r.status_code}")

                if r.status_code == 200:
                    success_count += 1
                elif r.status_code == 429:
                    rate_limited = True
                    log.info(f"Rate limit exceeded after {success_count} successful embedding requests")
                    break
                else:
                    raise AssertionError(
                        f"Unexpected status {r.status_code} at request {request_num}: {r.text[:200]}"
                    )

                time.sleep(0.1)

            assert success_count > 0, (
                f"Got 429 on request #{request_num} without any successful requests. "
                f"Configuration issue, not rate limit exhaustion."
            )
            assert rate_limited, (
                f"Expected 429 with {token_limit} tokens/{window} limit, "
                f"but got {success_count} successful requests without hitting limit"
            )

        finally:
            _delete_cr("maassubscription", subscription_name)
            _delete_cr("maasauthpolicy", auth_policy_name)
            time.sleep(RECONCILE_WAIT)

    def test_embedding_with_governance_200(self):
        """Embedding model with auth + subscription returns valid 200 response."""
        model = _get_cr("maasmodelref", EMBEDDING_MODEL_REF, namespace=MODEL_NAMESPACE)
        if not model:
            pytest.skip(f"MaaSModelRef {EMBEDDING_MODEL_REF} not deployed")

        auth_policy_name = "e2e-embedding-gov-auth"
        subscription_name = "e2e-embedding-gov-sub"

        try:
            _create_test_auth_policy(
                name=auth_policy_name,
                model_refs=[EMBEDDING_MODEL_REF],
                groups=["system:authenticated"],
            )
            _create_test_subscription(
                name=subscription_name,
                model_refs=[EMBEDDING_MODEL_REF],
                groups=["system:authenticated"],
                token_limit=1000,
                window="1m",
            )
            _wait_for_maas_auth_policy_phase(auth_policy_name, timeout=90, require_auth_policies=False)
            _wait_for_maas_subscription_phase(subscription_name, timeout=90)
            _wait_for_token_rate_limit_policy(
                EMBEDDING_MODEL_REF, model_namespace=MODEL_NAMESPACE, timeout=90
            )

            oc_token = _get_cluster_token()
            api_key = _create_api_key(
                oc_token,
                name=f"e2e-emb-gov-{uuid.uuid4().hex[:8]}",
                subscription=subscription_name,
            )

            r = _poll_status(
                api_key, 200, path=EMBEDDING_MODEL_PATH, model_name=EMBEDDING_MODEL_NAME,
                timeout=90, inference_fn=_embedding_inference,
            )
            data = r.json()
            assert "data" in data, f"Missing 'data' in response: {list(data.keys())}"
            assert len(data["data"]) > 0, "Empty data array"
            first = data["data"][0]
            assert "embedding" in first, f"Missing 'embedding' in data[0]: {list(first.keys())}"
            usage = data.get("usage", {})
            assert usage.get("prompt_tokens", 0) > 0, f"Expected prompt_tokens > 0, got {usage}"
            log.info(f"[embedding-gov] Full governance -> {r.status_code}, tokens={usage}")

        finally:
            _delete_cr("maassubscription", subscription_name)
            _delete_cr("maasauthpolicy", auth_policy_name)
            time.sleep(RECONCILE_WAIT)
