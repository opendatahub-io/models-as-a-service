import os
import requests

from conftest import TLS_VERIFY

TIMEOUT = (45, 45)  # (connect, read)

def _post(url: str, payload: dict, headers: dict, timeout_sec: int = 45) -> requests.Response:
    # TLS verification controlled by E2E_SKIP_TLS_VERIFY env var
    return requests.post(
        url,
        headers=headers,
        json=payload,
        timeout=(timeout_sec, timeout_sec),
        verify=TLS_VERIFY,
        stream=False,
    )

def chat(prompt: str, model_v1: str, headers: dict, model_name: str):
    url = f"{model_v1}/chat/completions"
    body = {"model": model_name, "messages": [{"role": "user", "content": prompt}]}
    return requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)

def completions(prompt: str, model_v1: str, headers: dict, model_name: str):
    url = f"{model_v1}/completions"
    body = {"model": model_name, "prompt": prompt, "max_tokens": 16}
    return requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)
