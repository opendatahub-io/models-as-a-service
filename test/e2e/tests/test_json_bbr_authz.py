"""
JSON body-based routing (BBR) authZ e2e for additional `/v1/*` JSON endpoints.

Regression: API key + subscription allow/deny on POST {gateway}/v1/<suffix> uses
the same Authorino path as chat (JSON `model` field → X-Gateway-Model-Name). Uses
the rerank e2e fixture mock, which serves multiple JSON suffixes.

Rerank request-count RateLimitPolicy is covered in test_rerank_inference.py.
Chat, completions, and embeddings are covered elsewhere; multipart is out of scope.
"""

import logging
import uuid

import pytest

from test_helper import (
    JSON_BBR_MODEL_CANONICAL_ID,
    JSON_BBR_MODEL_REF,
    MODEL_NAMESPACE,
    _create_api_key,
    _create_test_auth_policy,
    _create_test_subscription,
    _delete_governance_and_wait,
    _get_cluster_token,
    _get_cr,
    _json_bbr_post,
    _ns,
    _wait_for_maas_auth_policy_phase,
    _wait_for_subscription_inference_ready,
)

log = logging.getLogger(__name__)

pytestmark = [
    pytest.mark.xdist_group("api_json_bbr"),
    pytest.mark.serial,
    pytest.mark.full_suite,
]

AUTH_POLICY = "e2e-json-bbr-auth"
SUBSCRIPTION = "e2e-json-bbr-sub"


def _model_body(extra):
    return {"model": JSON_BBR_MODEL_CANONICAL_ID, **extra}


JSON_BBR_AUTH_CASES = [
    pytest.param(
        "v1/rerank",
        _model_body({"query": "q", "documents": ["a", "b"]}),
        id="rerank",
    ),
    pytest.param("v1/ocr", _model_body({"document": "page text"}), id="ocr"),
    pytest.param("v1/moderations", _model_body({"input": "hello"}), id="moderations"),
    pytest.param("v1/extraction", _model_body({"text": "extract me"}), id="extraction"),
    pytest.param("v1/decisions", _model_body({"context": "approve?"}), id="decisions"),
    pytest.param("v1/responses", _model_body({"input": "hello"}), id="responses"),
    pytest.param(
        "v1/images/generations",
        _model_body({"prompt": "a red circle", "n": 1}),
        id="images-generations",
    ),
    pytest.param("v1/audio/voices", _model_body({}), id="audio-voices"),
    pytest.param("v1/search/query", _model_body({"query": "find docs"}), id="search-query"),
]


@pytest.fixture(scope="module")
def json_bbr_governance():
    model = _get_cr("maasmodelref", JSON_BBR_MODEL_REF, namespace=MODEL_NAMESPACE)
    if not model:
        pytest.skip(f"MaaSModelRef {JSON_BBR_MODEL_REF} not deployed")

    try:
        _create_test_auth_policy(
            name=AUTH_POLICY,
            model_refs=[JSON_BBR_MODEL_REF],
            groups=["system:authenticated"],
        )
        _create_test_subscription(
            name=SUBSCRIPTION,
            model_refs=[JSON_BBR_MODEL_REF],
            groups=["system:authenticated"],
            token_limit=10000,
            window="1m",
        )
        _wait_for_maas_auth_policy_phase(AUTH_POLICY, require_auth_policies=False)
        _wait_for_subscription_inference_ready(
            SUBSCRIPTION, JSON_BBR_MODEL_REF, model_namespace=MODEL_NAMESPACE
        )
        api_key = _create_api_key(
            _get_cluster_token(),
            name=f"e2e-json-bbr-{uuid.uuid4().hex[:8]}",
            subscription=SUBSCRIPTION,
        )
        yield api_key
    finally:
        _delete_governance_and_wait(
            subscriptions=[(SUBSCRIPTION, _ns())],
            auth_policies=[(AUTH_POLICY, _ns())],
        )


class TestJsonBBRAuthorization:
    @pytest.mark.parametrize("path_suffix,body", JSON_BBR_AUTH_CASES)
    def test_json_bbr_allowed_200(self, json_bbr_governance, path_suffix, body):
        """Subscribed model + correct body model field → 200 on BBR URL."""
        r = _json_bbr_post(json_bbr_governance, path_suffix, body, bbr=True)
        log.info("[json-bbr] POST /%s -> %s", path_suffix, r.status_code)
        assert r.status_code == 200, f"Expected 200, got {r.status_code}: {r.text[:400]}"

    @pytest.mark.parametrize("path_suffix,body", JSON_BBR_AUTH_CASES)
    def test_json_bbr_wrong_model_403(self, json_bbr_governance, path_suffix, body):
        """Wrong model in JSON body → authZ denial (not routed as subscribed model)."""
        bad = dict(body)
        bad["model"] = "nonexistent-json-bbr-model"
        r = _json_bbr_post(json_bbr_governance, path_suffix, bad, bbr=True)
        log.info("[json-bbr-deny] POST /%s wrong model -> %s", path_suffix, r.status_code)
        assert r.status_code in (401, 403), (
            f"Expected 401/403 for wrong model, got {r.status_code}: {r.text[:300]}"
        )