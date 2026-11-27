"""Standalone maas-api model catalog test.

Runs after test_standalone_mode.py, once the MaaSModelRef CRD and a ready
MaaSModelRef have been installed and maas-api has restarted.
"""

import os

import pytest
import requests

STANDALONE_URL = os.environ.get("STANDALONE_MAAS_API_URL", "").rstrip("/")
EXPECTED = os.environ.get("STANDALONE_MODEL_CATALOG_EXPECTED") == "1"
MODEL_ID = os.environ.get("STANDALONE_MODEL_ID", "standalone-ready-model")
TIMEOUT = int(os.environ.get("STANDALONE_E2E_TIMEOUT", "30"))

pytestmark = [
    pytest.mark.skipif(
        not STANDALONE_URL or not EXPECTED,
        reason="standalone model catalog phase is not enabled",
    ),
    pytest.mark.xdist_group("standalone"),
    pytest.mark.full_suite,
]

IDENTITY_HEADERS = {
    "X-MaaS-Username": "standalone-e2e",
    "X-MaaS-Group": '["standalone-e2e"]',
}


def test_models_lists_ready_maas_modelref():
    create = requests.post(
        f"{STANDALONE_URL}/v1/api-keys",
        headers={**IDENTITY_HEADERS, "Content-Type": "application/json"},
        json={"name": "standalone-catalog-key"},
        timeout=TIMEOUT,
    )
    assert create.status_code == 201, create.text
    key = create.json()["key"]

    response = requests.get(
        f"{STANDALONE_URL}/v1/models",
        headers={**IDENTITY_HEADERS, "Authorization": f"Bearer {key}"},
        timeout=TIMEOUT,
    )
    assert response.status_code == 200, response.text
    ids = [model["id"] for model in response.json()["data"]]
    assert MODEL_ID in ids, f"{MODEL_ID!r} not in {ids!r}"
