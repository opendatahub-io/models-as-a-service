"""
Rerank JSON BBR governance e2e (RHAIRFE-3921).

Validates /v1/rerank body-based routing and AuthPolicy denial. Request-count
quota is out of scope until subscription RLP design lands.
"""

import logging
import uuid

import pytest
import requests

from test_helper import (
    MODEL_NAMESPACE,
    RERANK_MODEL_CANONICAL_ID,
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

pytestmark = [
    pytest.mark.xdist_group("api_json_bbr"),
    pytest.mark.serial,
    pytest.mark.full_suite,
]


class TestRerankGovernance:
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
            assert r.status_code == 403, (
                f"Expected 403 authZ denial for wrong model, got {r.status_code}: {r.text[:300]}"
            )
        finally:
            _delete_governance_and_wait(
                subscriptions=[(subscription_name, _ns())],
                auth_policies=[(auth_policy_name, _ns())],
            )
