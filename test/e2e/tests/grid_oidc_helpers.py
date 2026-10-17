"""Helpers for Grid OIDC system tests."""

from __future__ import annotations

import os
import time

import requests

from conftest import TLS_VERIFY
from test_helper import _request_with_gateway_retry


def required_grid_env(name: str) -> str:
    value = os.environ.get(name, "")
    assert value, f"{name} must be set when EXTERNAL_OIDC=true"
    return value


def request_grid_token(
    username: str,
    client_id: str | None = None,
    token_url: str | None = None,
) -> str:
    response = requests.post(
        token_url or required_grid_env("OIDC_TOKEN_URL"),
        data={
            "grant_type": "password",
            "client_id": client_id or required_grid_env("OIDC_CLIENT_ID"),
            "username": username,
            "password": "letmein",
        },
        timeout=30,
        verify=TLS_VERIFY,
    )
    assert response.status_code == 200, (
        f"Keycloak token request failed for {username}: "
        f"{response.status_code} {response.text}"
    )
    token = response.json().get("access_token")
    assert token, "Keycloak token response missing access_token"
    return token


def request_tenant_b_token(username: str) -> str:
    return request_grid_token(
        username,
        token_url=required_grid_env("OIDC_TOKEN_URL_TENANT_B"),
    )


def get_grid_subscriptions(maas_api_base_url: str, token: str) -> list[dict]:
    response = _request_with_gateway_retry(
        requests.get,
        f"{maas_api_base_url}/v1/subscriptions",
        headers={"Authorization": f"Bearer {token}"},
        timeout=30,
    )
    assert response.status_code == 200, (
        f"GET /v1/subscriptions failed for Grid token: "
        f"{response.status_code} {response.text}"
    )
    assert response.headers.get("Cache-Control") == "no-store"
    subscriptions = response.json()
    assert isinstance(subscriptions, list)
    return subscriptions


def find_grid_subscription(
    subscriptions: list[dict], subscription_name: str
) -> dict | None:
    return next(
        (
            subscription
            for subscription in subscriptions
            if subscription.get("subscription_id_header") == subscription_name
        ),
        None,
    )


def wait_for_grid_subscription(
    maas_api_base_url: str,
    username: str,
    subscription_name: str,
    *,
    timeout: int = 120,
    poll_interval: int = 2,
) -> dict:
    deadline = time.monotonic() + timeout
    last_subscriptions: list[dict] = []
    while time.monotonic() < deadline:
        token = request_grid_token(username)
        last_subscriptions = get_grid_subscriptions(maas_api_base_url, token)
        subscription = find_grid_subscription(last_subscriptions, subscription_name)
        if subscription is not None:
            return subscription
        time.sleep(poll_interval)

    names = [
        subscription.get("subscription_id_header", "<unnamed>")
        for subscription in last_subscriptions
    ]
    raise AssertionError(
        f"Grid subscription {subscription_name} was not returned within "
        f"{timeout}s; visible subscriptions: {names}"
    )
