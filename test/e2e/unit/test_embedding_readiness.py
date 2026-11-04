"""Both routing modes must eventually serve, never accept a persistent quota denial."""

from types import SimpleNamespace

import pytest
import requests
import test_embedding_inference as embedding
import test_helper as helper


@pytest.mark.parametrize("mode", ["path", "body"])
@pytest.mark.parametrize("recovers", [True, False])
def test_embedding_probes_the_requested_route_until_serving_or_budget_expires(monkeypatch, mode, recovers):
    elapsed = [0]
    monkeypatch.setattr(helper, "RECONCILE_WAIT", 1)
    monkeypatch.setattr(helper, "time", SimpleNamespace(
        time=lambda: elapsed[0], sleep=lambda _: elapsed.__setitem__(0, elapsed[0] + 30),
    ))
    monkeypatch.setattr(helper, "_gateway_url", lambda: "https://gateway.example")
    monkeypatch.setattr(embedding, "_gateway_url", lambda: "https://gateway.example")
    monkeypatch.setattr(embedding, "_get_cr", lambda *args, **kwargs: {"metadata": {"name": "model"}})
    calls = []

    def post(url, **kwargs):
        calls.append((url, kwargs["json"]))
        response = requests.Response()
        response.status_code = 200 if recovers and len(calls) > 1 else 429
        response._content = b'{"data":[{"embedding":[0.25]}],"usage":{"prompt_tokens":2}}'
        return response

    monkeypatch.setattr(embedding.requests, "post", post)
    test = embedding.TestEmbeddingPathRouting()
    method = test.test_embedding_path_based_200 if mode == "path" else test.test_embedding_bbr_llmisvc_200
    if recovers:
        method({"api_key": "test-key"})
    else:
        with pytest.raises(AssertionError, match="Expected 200 within"):
            method({"api_key": "test-key"})
    assert len(calls) == 2
    expected_path = f"{embedding.EMBEDDING_MODEL_PATH}/v1/embeddings" if mode == "path" else "/v1/embeddings"
    expected_model = embedding.EMBEDDING_MODEL_NAME if mode == "path" else embedding.EMBEDDING_MODEL_CANONICAL_ID
    assert all(url == f"https://gateway.example{expected_path}" for url, _ in calls)
    assert all(body["model"] == expected_model for _, body in calls)
