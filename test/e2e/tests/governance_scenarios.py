"""Backend-neutral catalog and API-key governance assertions for MaaS E2Es."""

from dataclasses import dataclass
import json
import time
from typing import Callable


@dataclass(frozen=True)
class GovernedModelCase:
    """The stable, externally visible part of a governed model fixture."""

    external_model_name: str
    maas_model_ref_name: str
    subscription_name: str
    inference_url: str
    expected_catalog_id: str
    expected_kind: str
    expected_owned_by: str
    api_key: str
    api_key_id: str


def _body(response) -> str:
    return (getattr(response, "text", "") or "")[:500]


def _model_ids(response):
    try:
        return [m.get("id") for m in response.json().get("data", [])]
    except (ValueError, AttributeError):
        return []


def _assert_catalog_case(case: GovernedModelCase, response, excluded_id: str):
    assert response.status_code == 200, (
        f"catalog for {case.expected_catalog_id} expected 200, got "
        f"{response.status_code}: {_body(response)}"
    )
    models = response.json().get("data") or []
    ids = {model.get("id") for model in models}
    assert case.expected_catalog_id in ids, (
        f"catalog missing {case.expected_catalog_id}; returned IDs: {sorted(ids)}"
    )
    assert excluded_id not in ids, (
        f"catalog for subscription {case.subscription_name} exposed {excluded_id}; "
        f"returned IDs: {sorted(ids)}"
    )
    model = next(item for item in models if item.get("id") == case.expected_catalog_id)
    assert model.get("kind") == case.expected_kind, model
    assert model.get("owned_by") == case.expected_owned_by, model
    assert model.get("ready") is True, model
    if case.expected_kind == "ExternalModel":
        assert model.get("url"), model
    subscriptions = [sub.get("name") for sub in model.get("subscriptions") or []]
    assert subscriptions == [case.subscription_name], (
        f"model {case.expected_catalog_id} subscriptions={subscriptions}, "
        f"expected only {case.subscription_name}"
    )


def _assert_inference_ok(case: GovernedModelCase, response):
    assert response.status_code == 200, (
        f"inference for {case.expected_catalog_id} expected 200, got "
        f"{response.status_code}: {_body(response)}"
    )
    try:
        choices = response.json().get("choices")
    except ValueError as exc:
        raise AssertionError(f"inference returned non-JSON body: {_body(response)}") from exc
    assert choices, f"inference response has no choices: {_body(response)}"


def assert_two_subscription_isolation(case_a, case_b, list_models: Callable, infer: Callable):
    """Assert symmetric discovery and inference isolation for two valid keys."""
    _assert_catalog_case(case_a, list_models(case_a.api_key), case_b.expected_catalog_id)
    _assert_catalog_case(case_b, list_models(case_b.api_key), case_a.expected_catalog_id)
    _assert_inference_ok(case_a, infer(case_a, case_a.api_key))
    _assert_inference_ok(case_b, infer(case_b, case_b.api_key))
    for source, target in ((case_a, case_b), (case_b, case_a)):
        response = infer(target, source.api_key)
        assert response.status_code == 403, (
            f"key for {source.subscription_name} could invoke {target.expected_catalog_id}: "
            f"HTTP {response.status_code}, body={_body(response)}"
        )


def assert_key_works(case, list_models: Callable, infer: Callable):
    """Require that a lifecycle key worked on both catalog and data planes."""
    response = list_models(case.api_key)
    _assert_catalog_case(case, response, "__no-excluded-model__")
    _assert_inference_ok(case, infer(case, case.api_key))


def wait_for_key_denied(
    case,
    list_models: Callable,
    infer: Callable,
    expected_model_statuses=(401, 403),
    expected_list_statuses=(401, 403),
    timeout=90,
):
    """Poll both planes until a key is denied, retaining useful diagnostics."""
    deadline = time.monotonic() + timeout
    last_model = last_list = None
    while time.monotonic() < deadline:
        last_model = infer(case, case.api_key)
        last_list = list_models(case.api_key)
        if (
            last_model.status_code in expected_model_statuses
            and last_list.status_code in expected_list_statuses
        ):
            return
        # A status that cannot represent propagation should fail immediately.
        invalid = {404, 429, 500, 501, 502, 503, 504}
        if last_model.status_code in invalid or last_list.status_code in invalid:
            raise AssertionError(
                f"denial for {case.expected_catalog_id} received non-auth response: "
                f"inference={last_model.status_code} {_body(last_model)}, "
                f"catalog={last_list.status_code} ids={_model_ids(last_list)} {_body(last_list)}"
            )
        time.sleep(2)
    raise AssertionError(
        f"timed out waiting for denial of {case.expected_catalog_id}: "
        f"inference={getattr(last_model, 'status_code', None)} {_body(last_model)}, "
        f"catalog={getattr(last_list, 'status_code', None)} "
        f"ids={_model_ids(last_list)} {_body(last_list)}"
    )


def wait_for_catalog_denied(case, list_models: Callable, expected_statuses=(403,), timeout=90):
    """Poll the catalog-only surface used by deleted-subscription scenarios."""
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        last = list_models(case.api_key)
        if last.status_code in expected_statuses:
            return
        if last.status_code in {404, 429, 500, 501, 502, 503, 504}:
            raise AssertionError(
                f"catalog denial for {case.expected_catalog_id} returned {last.status_code}: "
                f"ids={_model_ids(last)} {_body(last)}"
            )
        time.sleep(2)
    raise AssertionError(
        f"timed out waiting for catalog denial of {case.expected_catalog_id}: "
        f"HTTP {getattr(last, 'status_code', None)}, ids={_model_ids(last)} {_body(last)}"
    )
