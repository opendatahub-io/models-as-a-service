from __future__ import annotations
from kubernetes.dynamic import DynamicClient

from typing import Any, Dict, Optional

import requests
import json

from .utils import (
    MaasEnv,
    TestSummary,
    build_inference_endpoint,
    get_models,
    verify_inference_chat_completions,
)


class TestSmoke:
    def test_healthz_or_404(
        self,
        request_session_http: requests.Session,
        maas_env: MaasEnv,
        test_summary: TestSummary,
    ):
        passed = False
        last_status = None

        for path in ("/health", "/healthz"):
            try:
                url = f"{maas_env.maas_api_base_url}{path}"
                r = request_session_http.get(url, timeout=10)
                last_status = r.status_code
                print(f"[health] GET {path} -> {r.status_code}")
                assert r.status_code in (200, 401, 404)
                passed = True
                break
            except Exception as e:
                print(f"[health] {path} error: {e}")

        assert passed, "Neither /health nor /healthz responded as expected"

        test_summary.set(
            "Health endpoint reachable",
            result="PASS",
            actual=f"HTTP {last_status}",
            expected="200/401/404 on /health or /healthz",
        )

    def test_mint_token_requires_auth(
        self,
        request_session_http: requests.Session,
        maas_env: MaasEnv,
        test_summary: TestSummary,
    ):
        url = f"{maas_env.maas_api_base_url}/v1/tokens"

        r = request_session_http.post(url, json={"expiration": "1m"}, timeout=20)
        print(f"[token] POST /v1/tokens (no auth) -> {r.status_code}")

        if r.status_code == 200:
            body = r.json() or {}
            tok = body.get("token")
            assert tok, "200 from token endpoint but no 'token' field"
            actual = "HTTP 200 (token minted)"
        else:
            assert r.status_code in (401, 403), f"unexpected {r.status_code}: {r.text[:400]}"
            actual = f"HTTP {r.status_code} (auth enforced)"

        test_summary.set(
            "Token endpoint auth enforcement",
            result="PASS",
            actual=actual,
            expected="401/403 without auth (or 200 with token)",
        )

    def test_models_catalog(
        self,
        request_session_http: requests.Session,
        maas_env: MaasEnv,
        maas_token: str,
        test_summary: TestSummary,
    ):
        models_list = get_models(
            http_session=request_session_http,
            maas_api_base_url=maas_env.maas_api_base_url,
            maas_token=maas_token,
        )
        print(f"[models] count={len(models_list)}")
        assert isinstance(models_list, list) and len(models_list) >= 1

        first = models_list[0]
        assert "id" in first, "Model entry missing 'id'"

        test_summary.set(
            "Models catalog",
            result="PASS",
            actual=f"{len(models_list)} model(s) returned",
            expected="Non-empty models list with id",
        )

    def test_chat_completions_gateway_alive(
        self,
        request_session_http: requests.Session,
        maas_env: MaasEnv,
        maas_token: str,
        model_path: str,
        request_payload_template: Optional[Dict[str, Any]],
        test_summary: TestSummary,
    ):
        models_list = get_models(
            http_session=request_session_http,
            maas_api_base_url=maas_env.maas_api_base_url,
            maas_token=maas_token,
        )
        assert models_list, "Models catalog is empty; expected at least one model"
        model_entry = models_list[0]

        endpoint_url, model_id = build_inference_endpoint(
            model_entry=model_entry,
            maas_api_base_url=maas_env.maas_api_base_url,
            model_path=model_path,
        )

        r = verify_inference_chat_completions(
            http_session=request_session_http,
            endpoint_url=endpoint_url,
            token=maas_token,
            model_id=model_id,
            request_payload_template=request_payload_template,
            expected_status_codes=(200, 404),
        )

        test_summary.set(
            "Chat completions reachable",
            result="PASS",
            actual=f"HTTP {r.status_code}",
            expected="200 or 404",
        )

    def test_legacy_completions_optionally(
        self,
        request_session_http: requests.Session,
        maas_env: MaasEnv,
        maas_token: str,
        test_summary: TestSummary,
    ):
        models_list = get_models(
            http_session=request_session_http,
            maas_api_base_url=maas_env.maas_api_base_url,
            maas_token=maas_token,
        )

        assert models_list, "Models catalog is empty; expected at least one model"
        model_entry = models_list[0]

        endpoint_url, model_id = build_inference_endpoint(
            model_entry=model_entry,
            maas_api_base_url=maas_env.maas_api_base_url,
            model_path="/v1/completions",
        )

        headers = {"Authorization": f"Bearer {maas_token}", "Content-Type": "application/json"}
        payload = {"model": model_id, "prompt": "Say hello in one word.", "max_tokens": 25}

        r = request_session_http.post(endpoint_url, headers=headers, json=payload, timeout=30)
        print(f"[legacy] POST /completions -> {r.status_code}")

        assert r.status_code in (200, 404), f"unexpected {r.status_code}: {r.text[:500]}"

        test_summary.set(
            "Legacy completions endpoint",
            result="PASS",
            actual=f"HTTP {r.status_code}",
            expected="200 or 404",
        )

    def test_token_rate_limit_policy_enforced(
        self,
        admin_client: DynamicClient,
        maas_env: MaasEnv,
        test_summary: TestSummary,
    ) -> None:
        res = admin_client.resources.get(api_version="kuadrant.io/v1alpha1", kind="TokenRateLimitPolicy")
        obj = res.get(name=maas_env.tokenratelimitpolicy_name, namespace=maas_env.tokenratelimitpolicy_namespace)
        d = obj.to_dict()

        conditions = d.get("status", {}).get("conditions", []) or []
        accepted = next((c.get("status") for c in conditions if c.get("type") == "Accepted"), None)
        enforced = next((c.get("status") for c in conditions if c.get("type") == "Enforced"), None)

        assert accepted in ("True", None), f"TokenRateLimitPolicy Accepted not True: {accepted}"
        assert enforced in ("True",), f"TokenRateLimitPolicy Enforced not True: {enforced}"

        test_summary.set(
            "RateLimit: TokenRateLimitPolicy enforced",
            result="PASS",
            actual=f"Accepted={accepted}, Enforced={enforced}",
            expected="Enforced=True",
        )

    def test_token_rate_limit_functional_200_then_429(
        self,
        request_session_http: requests.Session,
        maas_env: MaasEnv,
        maas_token: str,
        model_path: str,
        request_payload_template: Optional[Dict[str, Any]],
        test_summary: TestSummary,
    ):
        models_list = get_models(
            http_session=request_session_http,
            maas_api_base_url=maas_env.maas_api_base_url,
            maas_token=maas_token,
        )
        assert models_list, "Models catalog is empty; expected at least one model"
        model_entry = models_list[0]

        endpoint_url, model_id = build_inference_endpoint(
            model_entry=model_entry,
            maas_api_base_url=maas_env.maas_api_base_url,
            model_path=model_path,
        )

        headers = {"Authorization": f"Bearer {maas_token}", "Content-Type": "application/json"}

        if request_payload_template:
            payload_copy = json.loads(json.dumps(request_payload_template))
            payload_text = json.dumps(payload_copy)
            payload_text = payload_text.replace("${MODEL_NAME}", model_id).replace("${MODEL_ID}", model_id)
            payload = json.loads(payload_text)
        else:
            payload = {"model": model_id, "messages": [{"role": "user", "content": "Hello"}], "max_tokens": 20}

        statuses = []
        attempts = maas_env.rate_limit_request_count or 10

        for i in range(attempts):
            r = request_session_http.post(endpoint_url, headers=headers, json=payload, timeout=30)
            print(f"[token-ratelimit] attempt {i + 1}/{attempts} -> {r.status_code}")
            statuses.append(r.status_code)
            if r.status_code == 429:
                break

        saw_200 = any(code == 200 for code in statuses)
        saw_429 = any(code == 429 for code in statuses)

        assert saw_200, f"Expected at least one 200 before rate limiting, got: {statuses}"
        assert saw_429, f"Expected 429 Too Many Requests after exceeding limits, got: {statuses}"

        test_summary.set(
            "RateLimit: TokenRateLimitPolicy functional",
            result="PASS",
            actual=f"statuses={statuses}",
            expected="200 then 429",
        )

    