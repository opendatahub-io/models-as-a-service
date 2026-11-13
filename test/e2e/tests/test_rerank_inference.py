"""
Rerank JSON BBR governance e2e (RHAISTRAT-2891 phase 1).

Validates /v1/rerank body-based routing, AuthPolicy denial, and interim
request-count RateLimitPolicy (429). Includes a chat BBR smoke check.
"""

import logging
import time
import uuid

import pytest
import requests

from test_helper import (
    MODEL_NAME,
    MODEL_NAMESPACE,
    MODEL_PATH,
    RATE_LIMIT_ENFORCEMENT_TIMEOUT,
    RERANK_MODEL_CANONICAL_ID,
    RERANK_MODEL_NAME,
    RERANK_MODEL_REF,
    TLS_VERIFY,
    _create_api_key,
    _create_test_auth_policy,
    _create_test_subscription,
    _delete_governance_and_wait,
    _gateway_url,
    _get_cluster_token,
    _get_cr,
    _ns,
    _poll_status,
    _rerank_inference,
    _wait_for_maas_auth_policy_phase,
    _wait_for_subscription_inference_ready,
)

log = logging.getLogger(__name__)


class TestRerankGovernance:
    pytestmark = pytest.mark.serial

    def test_rerank_bbr_200(self):
        model = _get_cr("maasmodelref", RERANK_MODEL_REF, namespace=MODEL_NAMESPACE)
        if not model:
            pytest.skip(f"MaaSModelRef {RERANK_MODEL_REF} not deployed")

        auth_policy_name = "e2e-rerank-gov-auth"
        subscription_name = "e2e-rerank-gov-sub"
        try:
            _create_test_auth_policy(
                name=auth_policy_name,
                model_refs=[RERANK_MODEL_REF],
                groups=["system:authenticated"],
            )
            _create_test_subscription(
                name=subscription_name,
                model_refs=[RERANK_MODEL_REF],
                groups=["system:authenticated"],
                token_limit=1000,
                window="1m",
            )
            _wait_for_maas_auth_policy_phase(auth_policy_name, require_auth_policies=False)
            _wait_for_subscription_inference_ready(
                subscription_name, RERANK_MODEL_REF, model_namespace=MODEL_NAMESPACE
            )
            api_key = _create_api_key(
                _get_cluster_token(),
                name=f"e2e-rerank-{uuid.uuid4().hex[:8]}",
                subscription=subscription_name,
            )
            _poll_status(
                api_key,
                200,
                timeout=120,
                inference_fn=lambda key, **_: _rerank_inference(
                    key, bbr=True, model_name=RERANK_MODEL_CANONICAL_ID
                ),
            )
            r = _rerank_inference(
                api_key, bbr=True, model_name=RERANK_MODEL_CANONICAL_ID
            )
            assert r.status_code == 200, f"Expected 200, got {r.status_code}: {r.text[:400]}"
            data = r.json()
            assert "results" in data, f"Unexpected body keys: {list(data.keys())}"
        finally:
            _delete_governance_and_wait(
                subscriptions=[(subscription_name, _ns())],
                auth_policies=[(auth_policy_name, _ns())],
            )

    def test_rerank_bbr_wrong_model_403(self):
        model = _get_cr("maasmodelref", RERANK_MODEL_REF, namespace=MODEL_NAMESPACE)
        if not model:
            pytest.skip(f"MaaSModelRef {RERANK_MODEL_REF} not deployed")

        auth_policy_name = "e2e-rerank-deny-auth"
        subscription_name = "e2e-rerank-deny-sub"
        try:
            _create_test_auth_policy(
                name=auth_policy_name,
                model_refs=[RERANK_MODEL_REF],
                groups=["system:authenticated"],
            )
            _create_test_subscription(
                name=subscription_name,
                model_refs=[RERANK_MODEL_REF],
                groups=["system:authenticated"],
                token_limit=100,
                window="1m",
            )
            _wait_for_maas_auth_policy_phase(auth_policy_name, require_auth_policies=False)
            _wait_for_subscription_inference_ready(
                subscription_name, RERANK_MODEL_REF, model_namespace=MODEL_NAMESPACE
            )
            api_key = _create_api_key(
                _get_cluster_token(),
                name=f"e2e-rerank-deny-{uuid.uuid4().hex[:8]}",
                subscription=subscription_name,
            )
            url = f"{_gateway_url()}/v1/rerank"
            headers = {
                "Authorization": f"Bearer {api_key}",
                "Content-Type": "application/json",
            }
            body = {
                "model": "nonexistent-rerank-model",
                "query": "q",
                "documents": ["a"],
            }
            r = requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)
            assert r.status_code in (401, 403), (
                f"Expected auth denial for wrong model, got {r.status_code}: {r.text[:300]}"
            )
        finally:
            _delete_governance_and_wait(
                subscriptions=[(subscription_name, _ns())],
                auth_policies=[(auth_policy_name, _ns())],
            )

    def test_rerank_request_rlp_429(self):
        model = _get_cr("maasmodelref", RERANK_MODEL_REF, namespace=MODEL_NAMESPACE)
        if not model:
            pytest.skip(f"MaaSModelRef {RERANK_MODEL_REF} not deployed")

        auth_policy_name = "e2e-rerank-trlp-auth"
        subscription_name = "e2e-rerank-trlp-sub"
        request_limit = 5
        total_requests = 20
        try:
            _create_test_auth_policy(
                name=auth_policy_name,
                model_refs=[RERANK_MODEL_REF],
                groups=["system:authenticated"],
            )
            _create_test_subscription(
                name=subscription_name,
                model_refs=[RERANK_MODEL_REF],
                groups=["system:authenticated"],
                token_limit=request_limit,
                window="1m",
            )
            _wait_for_maas_auth_policy_phase(auth_policy_name, require_auth_policies=False)
            _wait_for_subscription_inference_ready(
                subscription_name, RERANK_MODEL_REF, model_namespace=MODEL_NAMESPACE
            )
            api_key = _create_api_key(
                _get_cluster_token(),
                name=f"e2e-rerank-rlp-{uuid.uuid4().hex[:8]}",
                subscription=subscription_name,
            )

            rate_limited = False
            success_count = 0
            deadline = time.time() + RATE_LIMIT_ENFORCEMENT_TIMEOUT
            request_num = 0
            while request_num < total_requests or time.time() < deadline:
                request_num += 1
                r = _rerank_inference(
                    api_key, bbr=True, model_name=RERANK_MODEL_CANONICAL_ID
                )
                log.info("Rerank request %d: HTTP %d", request_num, r.status_code)
                if r.status_code == 200:
                    success_count += 1
                elif r.status_code == 429:
                    rate_limited = True
                    break
                elif r.status_code in (401, 403):
                    raise AssertionError(
                        f"Unexpected auth denial at request {request_num}: {r.text[:200]}"
                    )
                else:
                    raise AssertionError(
                        f"Unexpected status {r.status_code}: {r.text[:200]}"
                    )
                time.sleep(0.05)

            assert success_count > 0, "No successful rerank requests before limit"
            assert rate_limited, (
                f"Expected 429 with request limit {request_limit}, got {success_count} x 200"
            )
        finally:
            _delete_governance_and_wait(
                subscriptions=[(subscription_name, _ns())],
                auth_policies=[(auth_policy_name, _ns())],
            )

    def test_chat_bbr_regression_200(self):
        """Chat BBR still succeeds after rerank fixture deploy (shared gateway)."""
        url = f"{_gateway_url()}/v1/chat/completions"
        oc_token = _get_cluster_token()
        headers = {
            "Authorization": f"Bearer {oc_token}",
            "Content-Type": "application/json",
        }
        body = {
            "model": MODEL_NAME,
            "messages": [{"role": "user", "content": "ping"}],
        }
        r = requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)
        if r.status_code in (401, 403):
            pytest.skip("Cluster admin token not authorized for chat BBR smoke")
        assert r.status_code == 200, f"Chat BBR regression failed: {r.status_code}"
