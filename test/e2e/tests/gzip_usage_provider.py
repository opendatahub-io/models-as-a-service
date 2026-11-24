"""OpenAI-shaped chat provider that gzips its responses when asked to.

Not a test module: ``test_tenant_external_model_shared.py`` ships this file to the cluster
in a ConfigMap and runs it as the provider of a quota model. The simulators the suite
otherwise uses never compress, so they cannot show whether the gateway counts tokens from
a compressed response (RHAI-48855). Standard library only, so any Python image runs it.

Every chat completion carries ``usage.total_tokens`` (TOTAL_TOKENS) and says in
the assistant message what the provider was asked for and what it sent back, for example
``accept-encoding=gzip content-encoding=gzip``, so a test can tell a counted compressed
response from a provider that was never asked to compress.

A request with ``"stream": true`` is answered as server-sent events, the usage in the last
one. The provider waits STREAM_PAUSE seconds after the first event and flushes the gzip
stream after every event, so a test can tell a gateway that passes events on as they
arrive from one that waits for the end of the stream.

Listens on port 8000, with TLS when TLS_CERT and TLS_KEY name a certificate and its key.
"""

from __future__ import annotations

import gzip
import json
import os
import ssl
import time
import zlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

TOTAL_TOKENS = 2
STREAM_PAUSE = 1.0


class Provider(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _request_body(self) -> bytes:
        # Envoy sends a chunked body once an ext_proc filter has touched it.
        if "chunked" not in self.headers.get("Transfer-Encoding", "").lower():
            return self.rfile.read(int(self.headers.get("Content-Length") or 0))
        body = b""
        while True:
            size = int(self.rfile.readline().split(b";")[0].strip() or b"0", 16)
            if size == 0:
                while self.rfile.readline().strip():  # trailers
                    pass
                return body
            body += self.rfile.read(size)
            self.rfile.readline()  # CRLF after the chunk

    def _reply(self, payload: dict, *, compress: bool) -> None:
        body = json.dumps(payload).encode()
        if compress:
            body = gzip.compress(body)
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        if compress:
            self.send_header("Content-Encoding", "gzip")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _stream(self, events: list[dict], *, compress: bool) -> None:
        encoder = zlib.compressobj(wbits=zlib.MAX_WBITS | 16) if compress else None  # 16: gzip container
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        if compress:
            self.send_header("Content-Encoding", "gzip")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()

        def send(event: str, *, last: bool = False) -> None:
            data = f"data: {event}\n\n".encode()
            if encoder:
                # A sync flush ends on a byte boundary: all of the event can be decoded now.
                data = encoder.compress(data) + encoder.flush(zlib.Z_FINISH if last else zlib.Z_SYNC_FLUSH)
            self.wfile.write(f"{len(data):x}\r\n".encode() + data + b"\r\n")

        for index, event in enumerate(events):
            if index == 1:
                time.sleep(STREAM_PAUSE)
            send(json.dumps(event))
        send("[DONE]", last=True)
        self.wfile.write(b"0\r\n\r\n")

    def do_GET(self) -> None:  # noqa: N802 - http.server handler name
        self._reply({"status": "ok"}, compress=False)

    def do_POST(self) -> None:  # noqa: N802 - http.server handler name
        try:
            request = json.loads(self._request_body() or b"{}")
        except ValueError:
            request = {}
        # Every Accept-Encoding header, so a second one cannot hide behind the first.
        asked = ", ".join(self.headers.get_all("Accept-Encoding") or [])
        compress = "gzip" in asked.lower()
        said = [f"accept-encoding={asked or 'none'}", f" content-encoding={'gzip' if compress else 'identity'}"]
        completion = {
            "id": "chatcmpl-e2e-gzip",
            "created": 0,
            # Usage reports that do not name the model are dropped by the telemetry pipeline.
            "model": request.get("model") or "unknown",
        }
        usage = {
            "prompt_tokens": 1,
            "completion_tokens": TOTAL_TOKENS - 1,
            "total_tokens": TOTAL_TOKENS,
        }
        if request.get("stream"):
            chunk = {**completion, "object": "chat.completion.chunk"}
            first = {"index": 0, "delta": {"role": "assistant", "content": said[0]}, "finish_reason": None}
            second = {"index": 0, "delta": {"content": said[1]}, "finish_reason": "stop"}
            self._stream(
                [
                    {**chunk, "choices": [first]},
                    {**chunk, "choices": [second]},
                    # Where OpenAI reports the usage of a stream: a last event without choices.
                    {**chunk, "choices": [], "usage": usage},
                ],
                compress=compress,
            )
            return
        message = {"index": 0, "message": {"role": "assistant", "content": "".join(said)}, "finish_reason": "stop"}
        self._reply(
            {**completion, "object": "chat.completion", "choices": [message], "usage": usage},
            compress=compress,
        )


def main() -> None:
    server = ThreadingHTTPServer(("", 8000), Provider)
    cert, key = os.environ.get("TLS_CERT"), os.environ.get("TLS_KEY")
    if cert and key:
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        context.load_cert_chain(cert, key)
        server.socket = context.wrap_socket(server.socket, server_side=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
