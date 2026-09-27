"""A local fake webhook receiver for the forwarder tests.

It records every request (headers + body), can answer with a scripted list of
actions - a status code, or ``"truncate"`` for a lying Content-Length - and
verifies the HMAC-SHA256 signature with the same receiver-side scheme the
README documents.
"""

from __future__ import annotations

import hashlib
import hmac
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

SIGNATURE_HEADER = "X-MC-Agent-Signature"
TIMESTAMP_HEADER = "X-MC-Agent-Timestamp"
EVENT_ID_HEADER = "X-MC-Agent-Event-Id"
EVENT_TYPE_HEADER = "X-MC-Agent-Event-Type"
ATTEMPT_HEADER = "X-MC-Agent-Attempt"


def compute_signature(secret: str, timestamp: int | str, body: bytes) -> str:
    signed = f"{timestamp}.".encode("utf-8") + body
    digest = hmac.new(secret.encode("utf-8"), signed, hashlib.sha256).hexdigest()
    return f"sha256={digest}"


def verify_signature(secret: str, timestamp: int | str, body: bytes, signature: str) -> bool:
    return hmac.compare_digest(compute_signature(secret, timestamp, body), str(signature))


class _Handler(BaseHTTPRequestHandler):
    def do_POST(self) -> None:  # noqa: N802 - http.server's API
        owner: FakeWebhookReceiver = self.server.owner  # type: ignore[attr-defined]
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length)
        record = {
            "method": self.command,
            "path": self.path,
            # Header names are case-insensitive; lower-case them for easy lookup.
            "headers": {key.lower(): value for key, value in self.headers.items()},
            "body": body,
        }
        with owner.lock:
            owner.requests.append(record)
        if owner.delay:
            time.sleep(owner.delay)

        action = owner.next_action()
        if action == "truncate":
            # HTTP 200 with a lying Content-Length: once the connection closes,
            # the client's read() raises http.client.IncompleteRead.
            self.send_response(200)
            self.send_header("Content-Length", "100")
            self.end_headers()
            self.wfile.write(b"{")
            self.wfile.flush()
            self.close_connection = True
            return

        status = int(action)
        self.send_response(status)
        if 300 <= status < 400:
            self.send_header("Location", "/landing")
        self.send_header("Content-Length", "0")
        self.end_headers()

    def log_message(self, *args: Any) -> None:  # silence the test output
        pass


class FakeWebhookReceiver:
    """A threaded HTTP server on 127.0.0.1 that stands in for a receiver.

    ``actions`` is consumed in order; each entry is an HTTP status code or the
    string ``"truncate"``. When it runs out the receiver answers 200.
    """

    def __init__(self, actions: list[int | str] | None = None, delay: float = 0.0) -> None:
        self.actions: list[int | str] = list(actions or [])
        self.delay = delay
        self.requests: list[dict[str, Any]] = []
        self.lock = threading.Lock()
        self.server: ThreadingHTTPServer | None = None
        self.thread: threading.Thread | None = None
        self.port: int | None = None

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.port}/hook"

    def start(self) -> "FakeWebhookReceiver":
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
        self.server.owner = self  # type: ignore[attr-defined]
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        return self

    def stop(self) -> None:
        if self.server is not None:
            self.server.shutdown()
            self.server.server_close()
            self.server = None
        if self.thread is not None:
            self.thread.join(timeout=5)
            self.thread = None

    def next_action(self) -> int | str:
        with self.lock:
            if self.actions:
                return self.actions.pop(0)
        return 200

    def snapshot(self) -> list[dict[str, Any]]:
        with self.lock:
            return list(self.requests)
