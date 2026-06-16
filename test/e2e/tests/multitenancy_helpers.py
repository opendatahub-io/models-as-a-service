"""Shared helpers for multi-tenancy Phase 1 E2E tests (S1 / S11 / S6)."""

from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import time
import uuid
from typing import Any, Optional

import pytest
import requests

from test_helper import (
    MODEL_NAMESPACE,
    MODEL_REF,
    TIMEOUT,
    TLS_VERIFY,
    _apply_cr,
    _delete_cr,
    _ns,
    _wait_reconcile,
)

AITENANT_CRD = "aitenants.maas.opendatahub.io"
AITENANT_KIND = "aitenant"
TENANT_CR_NAME = "default-tenant"

LABEL_AI_GATEWAY_TENANT = "ai-gateway.opendatahub.io/tenant"
LABEL_MANAGED_BY_AITENANT = "maas.opendatahub.io/managed-by-aitenant"
LABEL_TENANT_NAME = "maas.opendatahub.io/tenant-name"
LABEL_TENANT_NAMESPACE = "maas.opendatahub.io/tenant-namespace"

FINALIZER_SUBSCRIPTION = "maas.opendatahub.io/subscription-cleanup"
FINALIZER_AUTHPOLICY = "maas.opendatahub.io/authpolicy-cleanup"

GATEWAY_AUTH_POLICY_NAME = "maas-gateway-auth"

AITENANT_NAMESPACE = os.environ.get("AITENANT_NAMESPACE", "ai-tenants")
GATEWAY_NAMESPACE = os.environ.get("GATEWAY_NAMESPACE", "openshift-ingress")
DEFAULT_GATEWAY_NAME = os.environ.get("GATEWAY_NAME", "maas-default-gateway")
AITENANT_GATEWAY_CLASS_NAME = os.environ.get("AITENANT_GATEWAY_CLASS_NAME", "openshift-default")
DEPLOYMENT_NAMESPACE = os.environ.get("DEPLOYMENT_NAMESPACE", "opendatahub")
OC_TIMEOUT = int(os.environ.get("E2E_OC_TIMEOUT", "60"))

DISCOVERY_ARG = "--enable-tenant-namespace-discovery=true"
SENSITIVE_FIELD_PATTERN = (
    r"password|passwd|pwd|secret|token|api[_-]?key|apikey|key|credential|authorization|cookie|session|dsn|uri|url"
)
SENSITIVE_FIELD_RE = re.compile(
    rf"({SENSITIVE_FIELD_PATTERN})",
    re.IGNORECASE,
)
SENSITIVE_VALUE_PATTERNS = (
    re.compile(r"\bBearer\s+[A-Za-z0-9._~+/=-]+", re.IGNORECASE),
    re.compile(r"\bsk-oai-[A-Za-z0-9._-]+", re.IGNORECASE),
    re.compile(r"\bpostgres(?:ql)?://[^\s\"']+", re.IGNORECASE),
    re.compile(r"\b[a-z][a-z0-9+.-]*://[^/\s:@\"'<>]+(?::[^@\s/\"'<>]+)?@[^\s\"'<>]+", re.IGNORECASE),
)


def _oc_bin() -> str:
    path = shutil.which("oc")
    if not path:
        raise RuntimeError("`oc` binary not found in PATH")
    return path


def _oc_run(args, *, input_text: Optional[str] = None, timeout: Optional[int] = None):
    return subprocess.run(
        [_oc_bin(), *args],
        input=input_text,
        capture_output=True,
        text=True,
        timeout=OC_TIMEOUT if timeout is None else timeout,
        check=False,
    )


def _oc_output_not_found(result) -> bool:
    combined = (result.stderr or "") + (result.stdout or "")
    return "(NotFound)" in combined or "not found" in combined.lower()


def env_bool(name: str, default: bool = False) -> bool:
    value = os.environ.get(name)
    if value is None:
        return default
    return value.lower() in {"1", "true", "yes", "y", "on"}


def external_oidc_from_env() -> Optional[dict[str, str]]:
    """Return the deployed OIDC config used by the default tenant, when enabled."""
    issuer = os.environ.get("OIDC_ISSUER_URL", "")
    if not env_bool("EXTERNAL_OIDC") or not issuer:
        return None
    return {
        "issuerUrl": issuer,
        "clientId": os.environ.get("OIDC_CLIENT_ID", "test-client"),
    }


def redact_sensitive(value: Any, *, max_length: int = 300) -> str:
    """Return a short string safe enough for CI assertion messages."""
    text = str(value)
    for pattern in SENSITIVE_VALUE_PATTERNS:
        text = pattern.sub("<redacted>", text)
    text = re.sub(
        rf'("(?:[^"]*(?:{SENSITIVE_FIELD_PATTERN})[^"]*)"\s*:\s*)"[^"]*"',
        r'\1"<redacted>"',
        text,
        flags=re.IGNORECASE,
    )
    text = re.sub(
        rf"('(?:[^']*(?:{SENSITIVE_FIELD_PATTERN})[^']*)'\s*:\s*)'[^']*'",
        r"\1'<redacted>'",
        text,
        flags=re.IGNORECASE,
    )
    if len(text) > max_length:
        return f"{text[:max_length]}...<truncated>"
    return text


def redact_mapping(mapping: dict[str, Any]) -> dict[str, Any]:
    return {
        key: "<redacted>" if SENSITIVE_FIELD_RE.search(key) else value
        for key, value in sorted(mapping.items())
    }


def response_summary(response: requests.Response, *, max_body: int = 300) -> str:
    return f"status={response.status_code} body={redact_sensitive(response.text, max_length=max_body)}"


def _apply(obj: dict) -> None:
    result = _oc_run(["apply", "-f", "-"], input_text=json.dumps(obj))
    if result.returncode != 0:
        raise RuntimeError(f"`oc apply` failed: {result.stderr.strip() or result.stdout.strip()}")


def _create_expect_failure(obj: dict) -> str:
    result = _oc_run(["create", "-f", "-"], input_text=json.dumps(obj))
    combined = (result.stderr or "") + (result.stdout or "")
    if result.returncode == 0:
        raise AssertionError(f"`oc create` unexpectedly succeeded: {combined}")
    return combined


def _delete(kind: str, name: str, namespace: Optional[str] = None, *, timeout: str = "60s") -> None:
    args = ["delete", kind, name, "--ignore-not-found", f"--timeout={timeout}"]
    if namespace:
        args.extend(["-n", namespace])
    result = _oc_run(args, timeout=OC_TIMEOUT + 30)
    if result.returncode != 0:
        raise RuntimeError(f"`oc {' '.join(args)}` failed: {result.stderr.strip() or result.stdout.strip()}")


def delete_best_effort(kind: str, name: str, namespace: Optional[str] = None, *, timeout: str = "60s") -> None:
    try:
        _delete(kind, name, namespace, timeout=timeout)
    except Exception as exc:  # noqa: BLE001
        print(f"[cleanup] failed to delete {kind}/{name}: {exc}")


def get_json_or_none(kind: str, name: str, namespace: Optional[str] = None) -> Optional[dict]:
    args = ["get", kind, name, "-o", "json"]
    if namespace:
        args.extend(["-n", namespace])
    result = _oc_run(args)
    if result.returncode == 0:
        return json.loads(result.stdout)
    if _oc_output_not_found(result):
        return None
    raise RuntimeError(f"`oc {' '.join(args)}` failed: {result.stderr.strip() or result.stdout.strip()}")


def list_json(kind: str, namespace: Optional[str] = None, *, labels: Optional[str] = None) -> list[dict]:
    args = ["get", kind, "-o", "json"]
    if namespace:
        args.extend(["-n", namespace])
    if labels:
        args.extend(["-l", labels])
    result = _oc_run(args)
    if result.returncode == 0:
        return json.loads(result.stdout).get("items") or []
    if _oc_output_not_found(result):
        return []
    raise RuntimeError(f"`oc {' '.join(args)}` failed: {result.stderr.strip() or result.stdout.strip()}")


def wait_for_json(
    kind: str,
    name: str,
    namespace: Optional[str] = None,
    *,
    predicate=None,
    timeout: int = 180,
    interval: int = 5,
) -> dict:
    deadline = time.time() + timeout
    last_obj = None
    while time.time() < deadline:
        obj = get_json_or_none(kind, name, namespace)
        if obj is not None:
            last_obj = obj
            if predicate is None or predicate(obj):
                return obj
        time.sleep(interval)
    raise AssertionError(
        f"{kind}/{name} in {namespace or '<cluster>'} did not satisfy condition. Last object: {last_obj}"
    )


def wait_for_not_found(
    kind: str,
    name: str,
    namespace: Optional[str] = None,
    *,
    timeout: int = 120,
    interval: int = 5,
) -> None:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if get_json_or_none(kind, name, namespace) is None:
            return
        time.sleep(interval)
    raise AssertionError(f"{kind}/{name} in {namespace or '<cluster>'} still exists")


def wait_for_finalizer(
    kind: str,
    name: str,
    namespace: str,
    finalizer: str,
    *,
    present: bool = True,
    timeout: int = 120,
    interval: int = 5,
) -> dict:
    def _predicate(obj: dict) -> bool:
        finalizers = obj.get("metadata", {}).get("finalizers") or []
        return (finalizer in finalizers) if present else (finalizer not in finalizers)

    return wait_for_json(kind, name, namespace, predicate=_predicate, timeout=timeout, interval=interval)


def wait_for_status_phase(
    kind: str,
    name: str,
    namespace: str,
    *,
    expected_phase: Optional[str | tuple[str, ...]] = None,
    timeout: int = 120,
    interval: int = 5,
) -> dict:
    def _predicate(obj: dict) -> bool:
        phase = (obj.get("status") or {}).get("phase")
        if expected_phase is None:
            return bool(phase)
        if isinstance(expected_phase, tuple):
            return phase in expected_phase
        return phase == expected_phase

    return wait_for_json(kind, name, namespace, predicate=_predicate, timeout=timeout, interval=interval)


def wait_for_deployment_available(name: str, namespace: str = DEPLOYMENT_NAMESPACE, *, timeout: int = 180) -> dict:
    def _predicate(obj: dict) -> bool:
        status = obj.get("status") or {}
        if status.get("availableReplicas", 0) < 1:
            return False
        for condition in status.get("conditions") or []:
            if condition.get("type") == "Available" and condition.get("status") == "True":
                return True
        return False

    return wait_for_json("deployment", name, namespace, predicate=_predicate, timeout=timeout)


def wait_for_annotation_contains(
    kind: str,
    name: str,
    namespace: str,
    annotation: str,
    expected_values: list[str],
    *,
    timeout: int = 180,
    interval: int = 5,
) -> list[str]:
    last_values: list[str] = []

    def _predicate(obj: dict) -> bool:
        nonlocal last_values
        annotations = obj.get("metadata", {}).get("annotations") or {}
        last_values = parse_annotation_list(annotations.get(annotation, ""))
        return all(value in last_values for value in expected_values)

    wait_for_json(kind, name, namespace, predicate=_predicate, timeout=timeout, interval=interval)
    return last_values


def controller_has_tenant_namespace_discovery() -> bool:
    result = _oc_run(["get", "deployment", "maas-controller", "-n", DEPLOYMENT_NAMESPACE, "-o", "json"])
    if result.returncode != 0:
        return False
    deployment = json.loads(result.stdout)
    containers = deployment.get("spec", {}).get("template", {}).get("spec", {}).get("containers") or []
    if not containers:
        return False
    args = containers[0].get("args") or []
    return DISCOVERY_ARG in args or "--enable-tenant-namespace-discovery" in args


def patch_controller_tenant_namespace_discovery(*, enabled: bool = True) -> None:
    """Patch maas-controller Deployment to enable or disable tenant namespace discovery."""
    result = _oc_run(["get", "deployment", "maas-controller", "-n", DEPLOYMENT_NAMESPACE, "-o", "json"])
    if result.returncode != 0:
        raise RuntimeError(f"failed to read maas-controller deployment: {result.stderr.strip()}")

    deployment = json.loads(result.stdout)
    containers = deployment["spec"]["template"]["spec"]["containers"]
    args = list(containers[0].get("args") or [])
    args = [a for a in args if not a.startswith("--enable-tenant-namespace-discovery")]
    if enabled:
        args.append(DISCOVERY_ARG)
    containers[0]["args"] = args

    patch = json.dumps(deployment)
    apply_result = _oc_run(["apply", "-f", "-"], input_text=patch)
    if apply_result.returncode != 0:
        raise RuntimeError(
            f"failed to patch maas-controller deployment: {apply_result.stderr.strip() or apply_result.stdout.strip()}"
        )

    rollout = _oc_run(
        ["rollout", "status", f"deployment/maas-controller", "-n", DEPLOYMENT_NAMESPACE, "--timeout=180s"],
        timeout=200,
    )
    if rollout.returncode != 0:
        raise RuntimeError(f"maas-controller rollout failed: {rollout.stderr.strip() or rollout.stdout.strip()}")


def require_tenant_namespace_discovery():
    if env_bool("ENABLE_TENANT_NAMESPACE_DISCOVERY"):
        if not controller_has_tenant_namespace_discovery():
            pytest.fail(
                "ENABLE_TENANT_NAMESPACE_DISCOVERY=true but maas-controller is missing "
                f"{DISCOVERY_ARG}; patch the deployment or run prow with multitenancy setup"
            )
        return
    if not controller_has_tenant_namespace_discovery():
        pytest.skip(
            f"maas-controller does not have {DISCOVERY_ARG}; "
            "set ENABLE_TENANT_NAMESPACE_DISCOVERY=true and patch the deployment to run multi-tenancy E2E"
        )


def require_s27_blocked_story_enabled(story: str) -> None:
    env_name = f"ENABLE_{story.upper()}_E2E"
    if not env_bool(env_name):
        pytest.skip(f"{story} E2E is present but gated until the backing story lands; set {env_name}=true")


def require_aitenant_crd():
    result = _oc_run(["get", "crd", AITENANT_CRD])
    if result.returncode != 0:
        if _oc_output_not_found(result):
            if env_bool("ENABLE_TENANT_NAMESPACE_DISCOVERY"):
                pytest.fail(f"Missing CRD {AITENANT_CRD}; multi-tenancy discovery E2E cannot run")
            pytest.skip(f"Missing CRD {AITENANT_CRD}; AITenant tests are not applicable")
        pytest.fail(f"`oc get crd {AITENANT_CRD}` failed: {result.stderr.strip() or result.stdout.strip()}")


def new_discovery_case(*, use_default_gateway: bool = False) -> dict[str, str]:
    suffix = uuid.uuid4().hex[:8]
    tenant_name = f"e2e-mt-{suffix}"
    return {
        "suffix": suffix,
        "tenant_ns": f"ai-tenant-{tenant_name}",
        "tenant_label_name": tenant_name,
        "gateway_name": DEFAULT_GATEWAY_NAME if use_default_gateway else tenant_name,
        "policy_name": f"e2e-policy-{suffix}",
        "subscription_name": f"e2e-sub-{suffix}",
    }


def new_named_tenant_case(prefix: str) -> dict[str, str]:
    """Create a stable-ish tenant case with a caller-provided DNS-safe prefix."""
    suffix = uuid.uuid4().hex[:6]
    tenant_name = f"{prefix}-{suffix}"
    return {
        "suffix": suffix,
        "tenant_ns": f"ai-tenant-{tenant_name}",
        "tenant_label_name": tenant_name,
        "gateway_name": tenant_name,
        "policy_name": f"{tenant_name}-policy",
        "subscription_name": f"{tenant_name}-sub",
    }


def admin_subject() -> str:
    whoami = _oc_run(["whoami"])
    if whoami.returncode == 0 and whoami.stdout.strip():
        return whoami.stdout.strip()
    raise RuntimeError(
        "oc whoami failed: "
        f"returncode={whoami.returncode} "
        f"stdout={whoami.stdout.strip()!r} "
        f"stderr={whoami.stderr.strip()!r}"
    )


def ensure_namespace(name: str, *, labels: Optional[dict[str, str]] = None) -> None:
    result = _oc_run(["create", "namespace", name])
    if result.returncode != 0 and "AlreadyExists" not in (result.stderr or "") and "already exists" not in (result.stderr or "").lower():
        raise RuntimeError(f"failed to create namespace {name}: {result.stderr.strip() or result.stdout.strip()}")
    if labels:
        patch = {"metadata": {"labels": labels}}
        patch_result = _oc_run(["patch", "namespace", name, "--type=merge", "-p", json.dumps(patch)])
        if patch_result.returncode != 0:
            raise RuntimeError(f"failed to label namespace {name}: {patch_result.stderr.strip()}")


def apply_discovery_labels(namespace: str, tenant_label_name: str) -> None:
    ensure_namespace(namespace)
    patch = {
        "metadata": {
            "labels": {
                LABEL_AI_GATEWAY_TENANT: tenant_label_name,
                LABEL_MANAGED_BY_AITENANT: "true",
                LABEL_TENANT_NAME: tenant_label_name,
                LABEL_TENANT_NAMESPACE: namespace,
            }
        }
    }
    result = _oc_run(["patch", "namespace", namespace, "--type=merge", "-p", json.dumps(patch)])
    if result.returncode != 0:
        raise RuntimeError(f"failed to apply discovery labels to {namespace}: {result.stderr.strip()}")


def remove_discovery_labels(namespace: str) -> None:
    patch = {
        "metadata": {
            "labels": {
                LABEL_AI_GATEWAY_TENANT: None,
                LABEL_MANAGED_BY_AITENANT: None,
                LABEL_TENANT_NAME: None,
                LABEL_TENANT_NAMESPACE: None,
            }
        }
    }
    result = _oc_run(["patch", "namespace", namespace, "--type=merge", "-p", json.dumps(patch)])
    if result.returncode != 0:
        raise RuntimeError(f"failed to remove discovery labels from {namespace}: {result.stderr.strip()}")


def apply_tenant_cr(
    namespace: str,
    gateway_name: str,
    *,
    gateway_namespace: str = GATEWAY_NAMESPACE,
    external_oidc: Optional[dict[str, str]] = None,
) -> None:
    spec: dict[str, Any] = {
        "gatewayRef": {
            "name": gateway_name,
            "namespace": gateway_namespace,
        }
    }
    if external_oidc is None and gateway_name == DEFAULT_GATEWAY_NAME:
        external_oidc = external_oidc_from_env()
    if external_oidc:
        spec["externalOIDC"] = external_oidc
    _apply(
        {
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "Tenant",
            "metadata": {"name": TENANT_CR_NAME, "namespace": namespace},
            "spec": spec,
        }
    )


def apply_gateway_fixture(gateway_name: str, *, fixture_label: str) -> None:
    _apply(
        {
            "apiVersion": "gateway.networking.k8s.io/v1",
            "kind": "Gateway",
            "metadata": {
                "name": gateway_name,
                "namespace": GATEWAY_NAMESPACE,
                "labels": {"e2e.maas.opendatahub.io/fixture": fixture_label},
            },
            "spec": {
                "gatewayClassName": AITENANT_GATEWAY_CLASS_NAME,
                "listeners": [{"name": "http", "port": 80, "protocol": "HTTP"}],
            },
        }
    )


def apply_aitenant(case: dict[str, str]) -> None:
    spec: dict[str, Any] = {
        "gateway": {"name": case["gateway_name"]},
        "rbac": {
            "admins": [{"kind": "User", "name": admin_subject()}],
        },
    }
    oidc = external_oidc_from_env()
    if oidc:
        spec["oidc"] = oidc

    _apply(
        {
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "AITenant",
            "metadata": {"name": case["tenant_label_name"], "namespace": AITENANT_NAMESPACE},
            "spec": spec,
        }
    )


def aitenant_ready(obj: dict) -> bool:
    status = obj.get("status") or {}
    if status.get("phase") != "Active":
        return False
    return any(
        cond.get("type") == "Ready" and cond.get("status") == "True"
        for cond in status.get("conditions") or []
    )


def bootstrap_aitenant_tenant(case: dict[str, str], *, use_default_gateway: bool = False) -> None:
    if not use_default_gateway:
        apply_gateway_fixture(case["gateway_name"], fixture_label=case["tenant_label_name"])
    apply_aitenant(case)
    wait_for_json(AITENANT_KIND, case["tenant_label_name"], AITENANT_NAMESPACE, predicate=aitenant_ready)
    wait_for_json("tenant", TENANT_CR_NAME, case["tenant_ns"])


def apply_maas_auth_policy(name: str, namespace: str, model_ref: str = MODEL_REF, model_namespace: str = MODEL_NAMESPACE) -> None:
    _apply_cr(
        {
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSAuthPolicy",
            "metadata": {"name": name, "namespace": namespace},
            "spec": {
                "modelRefs": [{"name": model_ref, "namespace": model_namespace}],
                "subjects": {"groups": [{"name": "system:authenticated"}]},
            },
        }
    )


def apply_maas_subscription(
    name: str,
    namespace: str,
    model_ref: str = MODEL_REF,
    model_namespace: str = MODEL_NAMESPACE,
    *,
    token_limit: int = 100,
    window: str = "1m",
    groups: Optional[list[str]] = None,
    users: Optional[list[str]] = None,
    priority: Optional[int] = None,
) -> None:
    owner: dict[str, Any] = {}
    if users:
        owner["users"] = users
    owner["groups"] = [{"name": group} for group in (groups or ["system:authenticated"])]
    spec: dict[str, Any] = {
        "owner": owner,
        "modelRefs": [
            {
                "name": model_ref,
                "namespace": model_namespace,
                "tokenRateLimits": [{"limit": token_limit, "window": window}],
            }
        ],
    }
    if priority is not None:
        spec["priority"] = int(priority)
    _apply_cr(
        {
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSSubscription",
            "metadata": {"name": name, "namespace": namespace},
            "spec": spec,
        }
    )


def delete_maas_auth_policy(name: str, namespace: str) -> None:
    _delete_cr("MaaSAuthPolicy", name, namespace)


def delete_maas_subscription(name: str, namespace: str) -> None:
    _delete_cr("MaaSSubscription", name, namespace)


def delete_namespace_best_effort(name: str) -> None:
    delete_best_effort("namespace", name, timeout="90s")


def get_cr_annotation(kind: str, name: str, namespace: str, key: str) -> str:
    obj = get_json_or_none(kind, name, namespace)
    if not obj:
        return ""
    annotations = obj.get("metadata", {}).get("annotations") or {}
    return annotations.get(key, "") or ""


def parse_annotation_list(value: str) -> list[str]:
    return [item.strip() for item in (value or "").split(",") if item.strip()]


def deployment_env(name: str, namespace: str = DEPLOYMENT_NAMESPACE, *, container_name: str = "maas-api") -> dict[str, str]:
    deployment = get_json_or_none("deployment", name, namespace)
    if not deployment:
        return {}
    containers = deployment.get("spec", {}).get("template", {}).get("spec", {}).get("containers") or []
    container = next((c for c in containers if c.get("name") == container_name), containers[0] if containers else {})
    return {entry.get("name"): entry.get("value") for entry in container.get("env") or [] if entry.get("name")}


def http_route_parent_refs(name: str, namespace: str = DEPLOYMENT_NAMESPACE) -> list[dict]:
    route = get_json_or_none("httproute", name, namespace)
    if not route:
        return []
    return ((route.get("spec") or {}).get("parentRefs") or [])


def http_route_backend_refs(name: str, namespace: str = DEPLOYMENT_NAMESPACE) -> list[dict]:
    route = get_json_or_none("httproute", name, namespace)
    if not route:
        return []
    refs: list[dict] = []
    for rule in (route.get("spec") or {}).get("rules") or []:
        refs.extend(rule.get("backendRefs") or [])
    return refs


def per_tenant_maas_api_names(tenant_name: str) -> dict[str, str]:
    return {
        "deployment": f"maas-api-{tenant_name}",
        "service": f"maas-api-{tenant_name}",
        "httproute": f"maas-api-{tenant_name}-route",
        "authpolicy": f"maas-api-{tenant_name}-auth",
    }


def get_gateway_authpolicy(namespace: str = GATEWAY_NAMESPACE, name: str = GATEWAY_AUTH_POLICY_NAME) -> Optional[dict]:
    return get_json_or_none("authpolicy", name, namespace)


def get_gateway_authpolicy_issuer(namespace: str = GATEWAY_NAMESPACE, name: str = GATEWAY_AUTH_POLICY_NAME) -> str:
    """Return OIDC issuerUrl from the gateway-scoped maas-gateway-auth policy (#912)."""
    ap = get_gateway_authpolicy(namespace, name)
    if not ap:
        return ""
    auth_rules = ((ap.get("spec") or {}).get("defaults") or {}).get("rules", {}).get("authentication") or {}
    oidc = auth_rules.get("oidc-identities") or {}
    jwt = oidc.get("jwt") or {}
    return jwt.get("issuerUrl") or ""


def get_gateway_authpolicy_target_ref(namespace: str = GATEWAY_NAMESPACE, name: str = GATEWAY_AUTH_POLICY_NAME) -> dict:
    ap = get_gateway_authpolicy(namespace, name)
    if not ap:
        return {}
    return (ap.get("spec") or {}).get("targetRef") or {}


def assert_no_per_model_authpolicy(model_ref: str, model_namespace: str = MODEL_NAMESPACE) -> None:
    """Gateway-only mode (#912) must not create maas-auth-{model} policies."""
    ap = get_json_or_none("authpolicy", f"maas-auth-{model_ref}", model_namespace)
    assert ap is None, f"legacy per-model AuthPolicy maas-auth-{model_ref} should not exist in gateway-only mode"


def get_kuadrant_authpolicy_issuer(model_ref: str, model_namespace: str = MODEL_NAMESPACE) -> str:
    """Deprecated alias: OIDC now lives on maas-gateway-auth, not per-model policies."""
    _ = (model_ref, model_namespace)
    return get_gateway_authpolicy_issuer()


def auth_can_create_maassubscription(subject: str, namespace: str) -> bool:
    result = _oc_run(
        [
            "auth",
            "can-i",
            "create",
            "maassubscriptions",
            "-n",
            namespace,
            f"--as={subject}",
        ]
    )
    return result.returncode == 0 and result.stdout.strip() == "yes"


def cleanup_discovery_case(case: dict[str, str], *, delete_gateway: bool = True) -> None:
    delete_best_effort(AITENANT_KIND, case["tenant_label_name"], AITENANT_NAMESPACE)
    delete_maas_auth_policy(case["policy_name"], case["tenant_ns"])
    delete_maas_subscription(case["subscription_name"], case["tenant_ns"])
    delete_namespace_best_effort(case["tenant_ns"])
    if delete_gateway and case["gateway_name"] != DEFAULT_GATEWAY_NAME:
        delete_best_effort("gateway", case["gateway_name"], GATEWAY_NAMESPACE)


def legacy_default_namespace() -> str:
    return _ns()


def tenant_api_base_url(slot: str) -> str:
    """Return a tenant maas-api base URL from explicit E2E env vars.

    Accepted env names for slot "TENANT_A":
    - MAAS_API_BASE_URL_TENANT_A
    - GATEWAY_HOST_TENANT_A (converted to https://host/maas-api unless INSECURE_HTTP=true)
    """
    normalized = slot.upper().replace("-", "_")
    explicit = os.environ.get(f"MAAS_API_BASE_URL_{normalized}")
    if explicit:
        return explicit.rstrip("/")
    host = os.environ.get(f"GATEWAY_HOST_{normalized}")
    if host:
        scheme = "http" if env_bool("INSECURE_HTTP") else "https"
        return f"{scheme}://{host}/maas-api"
    return ""


def require_tenant_api_base_urls(*slots: str) -> dict[str, str]:
    urls = {slot: tenant_api_base_url(slot) for slot in slots}
    missing = [slot for slot, url in urls.items() if not url]
    if missing:
        needed = ", ".join(f"MAAS_API_BASE_URL_{slot.upper().replace('-', '_')}" for slot in missing)
        pytest.fail(f"tenant maas-api URLs not configured; set {needed}")
    return urls


def bearer_headers(token: str) -> dict[str, str]:
    return {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}


def create_api_key_at(base_url: str, oc_token: str, name: str, *, subscription: Optional[str] = None) -> requests.Response:
    body: dict[str, str] = {"name": name}
    if subscription:
        body["subscription"] = subscription
    return requests.post(
        f"{base_url}/v1/api-keys",
        headers=bearer_headers(oc_token),
        json=body,
        timeout=TIMEOUT,
        verify=TLS_VERIFY,
    )


def get_api_key_at(base_url: str, oc_token: str, key_id: str) -> requests.Response:
    return requests.get(
        f"{base_url}/v1/api-keys/{key_id}",
        headers=bearer_headers(oc_token),
        timeout=TIMEOUT,
        verify=TLS_VERIFY,
    )


def search_api_keys_at(
    base_url: str,
    oc_token: str,
    *,
    subscription: Optional[str] = None,
    status: Optional[list[str]] = None,
) -> requests.Response:
    filters: dict[str, Any] = {"status": status or ["active"]}
    if subscription:
        filters["subscription"] = subscription
    return requests.post(
        f"{base_url}/v1/api-keys/search",
        headers=bearer_headers(oc_token),
        json={
            "filters": filters,
            "sort": {"by": "created_at", "order": "desc"},
            "pagination": {"limit": 50, "offset": 0},
        },
        timeout=TIMEOUT,
        verify=TLS_VERIFY,
    )


def validate_api_key_at(base_url: str, api_key: str) -> requests.Response:
    return requests.post(
        f"{base_url}/internal/v1/api-keys/validate",
        json={"key": api_key},
        timeout=TIMEOUT,
        verify=TLS_VERIFY,
    )


def select_subscription_at(
    base_url: str,
    api_key: str,
    username: str,
    groups: list[str],
    *,
    requested_subscription: Optional[str] = None,
    requested_model: Optional[str] = None,
) -> requests.Response:
    payload: dict[str, Any] = {"username": username, "groups": groups}
    if requested_subscription:
        payload["requestedSubscription"] = requested_subscription
    if requested_model:
        payload["requestedModel"] = requested_model
    return requests.post(
        f"{base_url}/internal/v1/subscriptions/select",
        headers={"Authorization": f"Bearer {api_key}"},
        json=payload,
        timeout=TIMEOUT,
        verify=TLS_VERIFY,
    )


def list_subscriptions_at(base_url: str, api_key: str) -> requests.Response:
    return requests.get(
        f"{base_url}/v1/subscriptions",
        headers=bearer_headers(api_key),
        timeout=TIMEOUT,
        verify=TLS_VERIFY,
    )
