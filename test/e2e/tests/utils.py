from __future__ import annotations

import base64
import json
import logging
import os
import re
import time
from contextlib import contextmanager
from dataclasses import dataclass
from typing import Any, Callable, Dict, Generator, List, Optional, Tuple
from urllib.parse import urlparse

import requests
from kubernetes.dynamic import DynamicClient
from ocp_resources.endpoints import Endpoints
from ocp_resources.group import Group
from ocp_resources.ingress_config_openshift_io import Ingress as IngressConfig
from ocp_resources.llm_inference_service import LLMInferenceService
from ocp_resources.resource import ResourceEditor
from requests import Response
from timeout_sampler import TimeoutSampler
from timeout_sampler import TimeoutExpiredError

from .resources.rate_limit_policy import RateLimitPolicy
from .resources.token_rate_limit_policy import TokenRateLimitPolicy

LOGGER = logging.getLogger(__name__)


@dataclass(frozen=True)
class MaasEnv:
    insecure_http: bool
    host: str
    maas_api_base_url: str

    maas_api_namespace: str

    gateway_namespace: str
    gateway_name: str

    maas_api_route_name: str

    authpolicy_namespace: str
    authpolicy_name: str

    tokenratelimitpolicy_namespace: str
    tokenratelimitpolicy_name: str

    request_ratelimitpolicy_namespace: str
    request_ratelimitpolicy_name: str

    rate_limit_request_count: int
    retry_wait_seconds: int

    metrics_url: Optional[str]


def env_bool(env_name: str, default_value: bool = False) -> bool:
    raw_value = os.getenv(env_name)
    if raw_value is None:
        return default_value
    return raw_value.strip().lower() in {"1", "true", "yes", "y", "on"}


def env_int(env_name: str, default_value: int) -> int:
    raw_value = os.getenv(env_name)
    if raw_value is None or raw_value.strip() == "":
        return default_value
    try:
        return int(raw_value.strip())
    except ValueError as exc:
        raise ValueError(f"Environment variable {env_name} must be an integer. Got: {raw_value!r}") from exc


def env_str(env_name: str, default_value: Optional[str] = None) -> Optional[str]:
    raw_value = os.getenv(env_name)
    if raw_value is None or raw_value.strip() == "":
        return default_value
    return raw_value.strip()


def normalize_url(raw_url: str) -> str:
    return raw_url.rstrip("/")


def wait_for_condition(
    description: str,
    condition_callable: Callable[[], Any],
    timeout_seconds: int = 600,
    sleep_seconds: int = 10,
) -> Any:
    last_value: Any = None
    try:
        for last_value in TimeoutSampler(
            wait_timeout=timeout_seconds,
            sleep=sleep_seconds,
            func=condition_callable,
        ):
            if last_value:
                return last_value
    except TimeoutExpiredError as exc:
        raise AssertionError(
            f"Timed out waiting for condition: {description}. Last value: {last_value!r}"
        ) from exc


def is_openshift_cluster(admin_client: DynamicClient) -> bool:
    try:
        admin_client.resources.get(api_version="route.openshift.io/v1", kind="Route")
        return True
    except Exception as exc:
        LOGGER.info(f"Route API not found: {exc}")
        return False


def host_from_ingress_domain(admin_client: DynamicClient) -> str:
    ingress_config = IngressConfig(name="cluster", client=admin_client, ensure_exists=True)
    domain = ingress_config.instance.spec.get("domain")
    assert domain, "Ingress 'cluster' missing spec.domain (ingresses.config.openshift.io)"
    return f"maas.{domain}"

def first_ready_llmisvc(
    admin_client: DynamicClient,
    namespace: str = "llm",
    label_selector: Optional[str] = None,
) -> Optional[LLMInferenceService]:
    for service in LLMInferenceService.get(
        client=admin_client,
        namespace=namespace,
        label_selector=label_selector,
    ):
        status = getattr(service.instance, "status", {}) or {}
        conditions = status.get("conditions", [])
        is_ready = any(
            condition.get("type") == "Ready" and condition.get("status") == "True"
            for condition in conditions
        )
        if is_ready:
            return service
    return None


def get_llm_inference_url_from_llmisvc(llm_service: LLMInferenceService) -> str:
    status = getattr(llm_service.instance, "status", {}) or {}
    url_value = status.get("url") or status.get("address") or ""
    if isinstance(url_value, str) and url_value:
        return url_value
    return ""


def detect_scheme_via_llmisvc(admin_client: DynamicClient, namespace: str = "llm") -> str:
    ready_service = first_ready_llmisvc(admin_client=admin_client, namespace=namespace)
    if not ready_service:
        return "https"

    service_url = get_llm_inference_url_from_llmisvc(llm_service=ready_service)
    parsed = urlparse(service_url)
    scheme = (parsed.scheme or "").lower()
    if scheme in ("http", "https"):
        return scheme
    return "https"


def count_running_pods(admin_client: DynamicClient, namespace: str, label_selector: Optional[str] = None) -> int:
    pod_resource = admin_client.resources.get(api_version="v1", kind="Pod")
    if label_selector:
        pod_list = pod_resource.get(namespace=namespace, label_selector=label_selector)
    else:
        pod_list = pod_resource.get(namespace=namespace)
    items = pod_list.to_dict().get("items", [])
    running = [item for item in items if item.get("status", {}).get("phase") == "Running"]
    return len(running)


def get_condition_status(resource_dict: dict, condition_type: str) -> Optional[str]:
    for condition in resource_dict.get("status", {}).get("conditions", []):
        if condition.get("type") == condition_type:
            return condition.get("status")
    return None


def any_parent_condition_true(resource_dict: dict, condition_type: str) -> bool:
    parents = resource_dict.get("status", {}).get("parents", [])
    for parent in parents:
        for condition in parent.get("conditions", []):
            if condition.get("type") == condition_type and condition.get("status") == "True":
                return True
    return False


def endpoints_have_ready_addresses(admin_client: DynamicClient, namespace: str, name: str) -> bool:
    endpoints = Endpoints(client=admin_client, name=name, namespace=namespace, ensure_exists=True)
    subsets = endpoints.instance.subsets
    if not subsets:
        return False
    return any(subset.addresses for subset in subsets)


def collect_infrastructure_diagnostics(admin_client: DynamicClient, maas_env: MaasEnv) -> Dict[str, Any]:
    diagnostics: Dict[str, Any] = {
        "maas_api_namespace": maas_env.maas_api_namespace,
        "gateway": f"{maas_env.gateway_namespace}/{maas_env.gateway_name}",
        "httproute": f"{maas_env.maas_api_namespace}/{maas_env.maas_api_route_name}",
        "authpolicy": f"{maas_env.authpolicy_namespace}/{maas_env.authpolicy_name}",
        "tokenratelimitpolicy": f"{maas_env.tokenratelimitpolicy_namespace}/{maas_env.tokenratelimitpolicy_name}",
        "request_ratelimitpolicy": f"{maas_env.request_ratelimitpolicy_namespace}/{maas_env.request_ratelimitpolicy_name}",
        "pods": {},
        "gateway_status": {},
        "httproute_status": {},
        "policies_status": {},
    }

    try:
        diagnostics["pods"]["maas_api_running"] = count_running_pods(
            admin_client=admin_client,
            namespace=maas_env.maas_api_namespace,
            label_selector="app.kubernetes.io/name=maas-api",
        )
    except Exception as exc:
        diagnostics["pods"]["maas_api_running_error"] = repr(exc)

    try:
        gateway_resource = admin_client.resources.get(api_version="gateway.networking.k8s.io/v1", kind="Gateway")
        gateway_obj = gateway_resource.get(name=maas_env.gateway_name, namespace=maas_env.gateway_namespace)
        gateway_dict = gateway_obj.to_dict()
        diagnostics["gateway_status"]["Accepted"] = get_condition_status(gateway_dict, "Accepted")
        diagnostics["gateway_status"]["Programmed"] = get_condition_status(gateway_dict, "Programmed")
    except Exception as exc:
        diagnostics["gateway_status"]["error"] = repr(exc)

    try:
        route_resource = admin_client.resources.get(api_version="gateway.networking.k8s.io/v1", kind="HTTPRoute")
        route_obj = route_resource.get(name=maas_env.maas_api_route_name, namespace=maas_env.maas_api_namespace)
        route_dict = route_obj.to_dict()
        diagnostics["httproute_status"]["ParentAccepted"] = any_parent_condition_true(route_dict, "Accepted")
    except Exception as exc:
        diagnostics["httproute_status"]["error"] = repr(exc)

    try:
        authpolicy_resource = admin_client.resources.get(api_version="kuadrant.io/v1", kind="AuthPolicy")
        authpolicy_obj = authpolicy_resource.get(name=maas_env.authpolicy_name, namespace=maas_env.authpolicy_namespace)
        diagnostics["policies_status"]["AuthPolicyAccepted"] = get_condition_status(authpolicy_obj.to_dict(), "Accepted")
    except Exception as exc:
        diagnostics["policies_status"]["AuthPolicyError"] = repr(exc)

    try:
        token_policy_resource = admin_client.resources.get(api_version="kuadrant.io/v1alpha1", kind="TokenRateLimitPolicy")
        token_policy_obj = token_policy_resource.get(
            name=maas_env.tokenratelimitpolicy_name,
            namespace=maas_env.tokenratelimitpolicy_namespace,
        )
        token_policy_dict = token_policy_obj.to_dict()
        diagnostics["policies_status"]["TokenRateLimitPolicyAccepted"] = get_condition_status(token_policy_dict, "Accepted")
        diagnostics["policies_status"]["TokenRateLimitPolicyEnforced"] = get_condition_status(token_policy_dict, "Enforced")
    except Exception as exc:
        diagnostics["policies_status"]["TokenRateLimitPolicyError"] = repr(exc)

    try:
        request_policy_resource = admin_client.resources.get(api_version="kuadrant.io/v1", kind="RateLimitPolicy")
        request_policy_obj = request_policy_resource.get(
            name=maas_env.request_ratelimitpolicy_name,
            namespace=maas_env.request_ratelimitpolicy_namespace,
        )
        request_policy_dict = request_policy_obj.to_dict()
        diagnostics["policies_status"]["RateLimitPolicyAccepted"] = get_condition_status(request_policy_dict, "Accepted")
        diagnostics["policies_status"]["RateLimitPolicyEnforced"] = get_condition_status(request_policy_dict, "Enforced")
    except Exception as exc:
        diagnostics["policies_status"]["RateLimitPolicyError"] = repr(exc)

    return diagnostics


def format_diagnostics_for_log(diagnostics: Dict[str, Any]) -> str:
    try:
        return json.dumps(diagnostics, indent=2, sort_keys=True)
    except Exception:
        return str(diagnostics)


def safe_json(response: Response) -> Dict[str, Any]:
    try:
        body = response.json()
        if isinstance(body, dict):
            return body
        return {"_non_dict_json": body}
    except Exception:
        return {}


def maas_auth_headers(token: str) -> Dict[str, str]:
    return {"Authorization": f"Bearer {token}"}


def mint_maas_token(
    http_session: requests.Session,
    maas_api_base_url: str,
    ocp_bearer_token: str,
    retry_wait_seconds: int = 60,
    minutes: int = 10,
) -> str:
    token_url = f"{normalize_url(maas_api_base_url)}/v1/tokens"
    request_payload = {"expiration": f"{minutes}m"}
    headers = {**maas_auth_headers(token=ocp_bearer_token), "Content-Type": "application/json"}

    def send_request() -> requests.Response:
        return http_session.post(token_url, headers=headers, json=request_payload, timeout=30)

    first_response = send_request()
    if first_response.ok:
        token_value = safe_json(first_response).get("token")
        if not token_value:
            raise AssertionError(f"Token mint succeeded but 'token' missing. Response: {first_response.text[:500]}")
        return str(token_value)

    LOGGER.warning(
        f"First token mint attempt failed (HTTP {first_response.status_code}). Waiting {retry_wait_seconds}s and retrying."
    )
    time.sleep(retry_wait_seconds)

    second_response = send_request()
    if second_response.ok:
        token_value = safe_json(second_response).get("token")
        if not token_value:
            raise AssertionError(f"Token mint succeeded but 'token' missing. Response: {second_response.text[:500]}")
        return str(token_value)

    raise AssertionError(
        "Failed to mint MaaS token after retry. "
        f"First: HTTP {first_response.status_code} {first_response.text[:200]} | "
        f"Second: HTTP {second_response.status_code} {second_response.text[:200]}"
    )


def get_models(http_session: requests.Session, maas_api_base_url: str, maas_token: str) -> List[Dict[str, Any]]:
    models_url = f"{normalize_url(maas_api_base_url)}/v1/models"
    headers = {"Authorization": f"Bearer {maas_token}"}
    response = http_session.get(models_url, headers=headers, timeout=30)
    if response.status_code != 200:
        raise AssertionError(f"/v1/models failed: HTTP {response.status_code}. Body: {response.text[:500]}")

    response_json = safe_json(response)
    models_list = response_json.get("data") or response_json.get("models") or []
    if not isinstance(models_list, list):
        raise AssertionError(f"Unexpected /v1/models shape. Expected list, got: {type(models_list)}")
    return models_list


def build_inference_endpoint(model_entry: Dict[str, Any], maas_api_base_url: str, model_path: str) -> Tuple[str, str]:
    model_id = str(model_entry.get("id") or "")
    assert model_id, "Model entry missing 'id'"

    model_url = model_entry.get("url")
    if model_url:
        base_model_url = normalize_url(str(model_url))
    else:
        base_without_maas_api = normalize_url(maas_api_base_url)
        if base_without_maas_api.endswith("/maas-api"):
            base_without_maas_api = base_without_maas_api[: -len("/maas-api")]
        base_model_url = f"{base_without_maas_api}/llm/{model_id}"

    normalized_path = model_path if model_path.startswith("/") else f"/{model_path}"
    endpoint_url = f"{base_model_url}{normalized_path}"
    return endpoint_url, model_id


def verify_inference_chat_completions(
    http_session: requests.Session,
    endpoint_url: str,
    token: str,
    model_id: str,
    *,
    request_payload_template: Optional[Dict[str, Any]] = None,
    max_tokens: int = 50,
    timeout_seconds: int = 60,
    expected_status_codes: Tuple[int, ...] = (200,),
) -> Response:
    headers = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}

    if request_payload_template:
        payload_copy = json.loads(json.dumps(request_payload_template))
        payload_text = json.dumps(payload_copy)
        payload_text = payload_text.replace("${MODEL_NAME}", model_id).replace("${MODEL_ID}", model_id)
        payload = json.loads(payload_text)
    else:
        payload = {"model": model_id, "messages": [{"role": "user", "content": "Hello"}], "max_tokens": max_tokens}

    response = http_session.post(url=endpoint_url, headers=headers, json=payload, timeout=timeout_seconds)

    assert response.status_code in expected_status_codes, (
        f"Inference failed: HTTP {response.status_code}. Body: {response.text[:500]} "
        f"(url={endpoint_url}), expected one of {expected_status_codes}"
    )

    if response.status_code == 200:
        body = safe_json(response)
        if "choices" in body:
            choices = body.get("choices") or []
            assert isinstance(choices, list) and choices, "Inference response has no choices"

        total_tokens = get_total_tokens(response=response, fail_if_missing=False)
        assert total_tokens is not None, (
            "Token usage not found in inference response (header x-odhu-usage-total-tokens or JSON usage.total_tokens)."
        )

    return response


def verify_unauth_rejected(
    http_session: requests.Session,
    endpoint_url: str,
    model_id: str,
    *,
    request_payload_template: Optional[Dict[str, Any]] = None,
    max_tokens: int = 50,
) -> Response:
    headers = {"Content-Type": "application/json"}

    if request_payload_template:
        payload_copy = json.loads(json.dumps(request_payload_template))
        payload_text = json.dumps(payload_copy)
        payload_text = payload_text.replace("${MODEL_NAME}", model_id).replace("${MODEL_ID}", model_id)
        payload = json.loads(payload_text)
    else:
        payload = {"model": model_id, "messages": [{"role": "user", "content": "Hello"}], "max_tokens": max_tokens}

    response = http_session.post(url=endpoint_url, headers=headers, json=payload, timeout=30)
    assert response.status_code in (401, 403), (
        f"Expected 401/403 without MaaS token, got {response.status_code}. Body: {response.text[:200]}"
    )
    return response


def b64url_decode(encoded_str: str) -> bytes:
    padding = "=" * (-len(encoded_str) % 4)
    padded = (encoded_str + padding).encode("utf-8")
    return base64.urlsafe_b64decode(padded)


def decode_jwt_payload(jwt_token: str) -> Dict[str, Any]:
    parts = jwt_token.split(".")
    if len(parts) < 2:
        raise AssertionError("Invalid JWT token format")
    payload_bytes = b64url_decode(parts[1])
    decoded = payload_bytes.decode("utf-8")
    payload = json.loads(decoded)
    if not isinstance(payload, dict):
        raise AssertionError("JWT payload is not a JSON object")
    return payload


def extract_tier_from_subject(subject: str) -> Optional[str]:
    match_obj = re.search(r"tier-([^:]+):", subject)
    if not match_obj:
        return None
    return match_obj.group(1)


def maas_token_ratelimitpolicy_limits() -> Dict[str, Any]:
    return {
        "enterprise-user-tokens": {
            "counters": [{"expression": "auth.identity.userid"}],
            "rates": [{"limit": 240, "window": "1m"}],
            "when": [{"predicate": 'auth.identity.tier == "enterprise"'}],
        },
        "free-user-tokens": {
            "counters": [{"expression": "auth.identity.userid"}],
            "rates": [{"limit": 60, "window": "1m"}],
            "when": [{"predicate": 'auth.identity.tier == "free"'}],
        },
        "premium-user-tokens": {
            "counters": [{"expression": "auth.identity.userid"}],
            "rates": [{"limit": 120, "window": "1m"}],
            "when": [{"predicate": 'auth.identity.tier == "premium"'}],
        },
    }


def maas_request_ratelimitpolicy_limits() -> Dict[str, Any]:
    return {
        "enterprise": {
            "counters": [{"expression": "auth.identity.userid"}],
            "rates": [{"limit": 50, "window": "2m"}],
            "when": [{"predicate": 'auth.identity.tier == "enterprise"'}],
        },
        "free": {
            "counters": [{"expression": "auth.identity.userid"}],
            "rates": [{"limit": 5, "window": "1m"}],
            "when": [{"predicate": 'auth.identity.tier == "free"'}],
        },
        "premium": {
            "counters": [{"expression": "auth.identity.userid"}],
            "rates": [{"limit": 8, "window": "1m"}],
            "when": [{"predicate": 'auth.identity.tier == "premium"'}],
        },
    }


@contextmanager
def patched_gateway_rate_limits(
    *,
    admin_client: DynamicClient,
    namespace: str,
    token_policy_name: str,
    request_policy_name: str,
) -> Generator[None, None, None]:
    token_policy = TokenRateLimitPolicy(client=admin_client, name=token_policy_name, namespace=namespace, ensure_exists=True)
    request_policy = RateLimitPolicy(client=admin_client, name=request_policy_name, namespace=namespace, ensure_exists=True)

    LOGGER.info(f"Patching TokenRateLimitPolicy {namespace}/{token_policy_name} spec.limits")
    with ResourceEditor(patches={token_policy: {"spec": {"limits": maas_token_ratelimitpolicy_limits()}}}):
        token_policy.wait_for_condition(condition="Enforced", status="True", timeout=60)

        LOGGER.info(f"Patching RateLimitPolicy {namespace}/{request_policy_name} spec.limits")
        with ResourceEditor(patches={request_policy: {"spec": {"limits": maas_request_ratelimitpolicy_limits()}}}):
            request_policy.wait_for_condition(condition="Enforced", status="True", timeout=60)
            yield

    LOGGER.info("Restored original Kuadrant policy specs")


def get_total_tokens(response: Response, *, fail_if_missing: bool = False) -> Optional[int]:
    header_value = response.headers.get("x-odhu-usage-total-tokens")
    if header_value is not None:
        try:
            return int(header_value)
        except (TypeError, ValueError):
            if fail_if_missing:
                raise AssertionError(f"Token usage header not parseable; headers={dict(response.headers)}") from None
            return None

    body = safe_json(response)
    usage = body.get("usage")
    if isinstance(usage, dict):
        total_tokens = usage.get("total_tokens")
        if isinstance(total_tokens, int):
            return total_tokens

    if fail_if_missing:
        raise AssertionError("Token usage not found in header or JSON body")
    return None


@contextmanager
def create_openshift_group(
    admin_client: DynamicClient,
    group_name: str,
    users: Optional[List[str]] = None,
) -> Generator[Group, None, None]:
    with Group(client=admin_client, name=group_name, users=users or [], wait_for_resource=True) as group:
        LOGGER.info(f"Created Group {group_name} with users {users or []}")
        yield group

@dataclass
class TestSummaryRow:
    testcase: str
    result: str          
    actual: str
    expected: str


class TestSummary:
    def __init__(self) -> None:
        self.rows: List[TestSummaryRow] = []

    def set(
        self,
        testcase: str,
        *,
        result: str,
        actual: str,
        expected: str,
    ) -> None:
        for i, r in enumerate(self.rows):
            if r.testcase == testcase:
                self.rows[i] = TestSummaryRow(testcase, result, actual, expected)
                return
        self.rows.append(TestSummaryRow(testcase, result, actual, expected))

    def render(self) -> str:
        def icon(res: str) -> str:
            r = (res or "").strip().upper()
            if r == "PASS":
                return "✅"
            if r == "FAIL":
                return "❌"
            if r in ("WARN", "WARNING"):
                return "⚠️"
            return "ℹ️"

        blocks: List[str] = []
        for r in self.rows:
            blocks.append(
                "\n".join(
                    [
                        f"{icon(r.result)} {r.testcase} — {r.result}",
                        f"   Actual:   {r.actual}",
                        f"   Expected: {r.expected}",
                    ]
                )
            )
        return "\n\n".join(blocks)
