from __future__ import annotations

import base64
import hashlib
import hmac
import http.client
import json
import socket
import tempfile
import threading
import time
import unittest
from dataclasses import dataclass
from http.server import ThreadingHTTPServer
from pathlib import Path

from worker.arasaka_worker.server import WorkerService
from worker.arasaka_worker.tokens import PRESET, ReplayStore, TokenError, TokenVerifier


SECRET = bytes(range(32))


def b64url(value: bytes) -> str:
    return base64.urlsafe_b64encode(value).decode("ascii").rstrip("=")


def make_token(nonce: str | None = None) -> tuple[str, dict[str, object]]:
    claims: dict[str, object] = {
        "v": 1,
        "media_id": "test-media-1",
        "source_size": 8,
        "source_mtime_ns": 1770000000000000000,
        "duration_ms": 2000,
        "offset_seconds": 0,
        "preset": PRESET,
        "exp": int(time.time()) + 60,
        "nonce": nonce or b64url(b"unique-nonce-for-test"),
    }
    payload = b64url(json.dumps(claims, separators=(",", ":")).encode("utf-8"))
    signature = b64url(hmac.new(SECRET, payload.encode("ascii"), hashlib.sha256).digest())
    return f"Bearer {payload}.{signature}", claims


@dataclass(frozen=True)
class TestSettings:
    bind: str
    port: int
    ffmpeg: str
    secret_file: Path
    replay_db: Path
    max_input_bytes: int = 1024
    max_duration_seconds: int = 120


class FakeBackend:
    def __init__(self):
        self.first_chunk = threading.Event()
        self.continue_stream = threading.Event()

    def ready(self) -> bool:
        return True

    def run(self, claims, source, content_length: int, write_chunk):
        data = source.read(content_length)
        if len(data) != content_length:
            raise RuntimeError("short fake input")
        output = b"mp3:" + data
        write_chunk(output[:4])
        self.first_chunk.set()
        self.continue_stream.wait(2)
        write_chunk(output[4:])
        return hashlib.sha256(output).hexdigest(), len(output)


class WorkerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        base = Path(self.temp.name)
        self.secret_file = base / "secret.key"
        self.secret_file.write_text(SECRET.hex(), encoding="ascii")
        self.settings = TestSettings("127.0.0.1", 0, "unused", self.secret_file, base / "state" / "replay.sqlite3")
        self.backend = FakeBackend()
        verifier = TokenVerifier(
            self.secret_file,
            ReplayStore(self.settings.replay_db),
            max_input_bytes=self.settings.max_input_bytes,
            max_duration_seconds=self.settings.max_duration_seconds,
        )
        self.service = WorkerService(self.settings, backend=self.backend, verifier=verifier)
        self.httpd = ThreadingHTTPServer((self.settings.bind, self.settings.port), self.service.handler())
        self.httpd.daemon_threads = True
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)
        self.thread.start()
        self.address, self.port = self.httpd.server_address

    def tearDown(self):
        self.backend.continue_stream.set()
        self.httpd.shutdown()
        self.httpd.server_close()
        self.thread.join(timeout=2)
        self.temp.cleanup()

    def request(self, path: str, *, token: str | None = None, body: bytes = b""):
        conn = http.client.HTTPConnection(self.address, self.port, timeout=3)
        headers = {"Content-Length": str(len(body)), "Content-Type": "application/octet-stream"} if body else {}
        if token:
            headers["Authorization"] = token
        conn.request("POST" if path == "/v1/transcode" else "GET", path, body=body, headers=headers)
        return conn, conn.getresponse()

    def test_health_and_readiness_are_separate(self):
        conn = http.client.HTTPConnection(self.address, self.port, timeout=3)
        conn.request("GET", "/health")
        health = conn.getresponse()
        self.assertEqual(health.status, 200)
        self.assertEqual(json.loads(health.read()), {"status": "ok"})
        conn.request("GET", "/ready")
        ready = conn.getresponse()
        self.assertEqual(ready.status, 200)
        self.assertEqual(json.loads(ready.read()), {"ready": True})
        conn.close()

    def test_streaming_output_and_one_use_token(self):
        token, _ = make_token()
        body = b"musicdat"
        conn = http.client.HTTPConnection(self.address, self.port, timeout=3)
        conn.request(
            "POST",
            "/v1/transcode",
            body=body,
            headers={"Authorization": token, "Content-Type": "application/octet-stream"},
        )
        response = conn.getresponse()
        self.assertEqual(response.status, 200)
        self.assertTrue(response.chunked)
        self.assertTrue(self.backend.first_chunk.wait(1))
        self.assertEqual(response.read(4), b"mp3:")
        self.backend.continue_stream.set()
        self.assertEqual(response.read(), body)
        conn.close()

        replay_conn, replay = self.request("/v1/transcode", token=token, body=body)
        self.assertEqual(replay.status, 401)
        replay.read()
        replay_conn.close()

    def test_bad_signature_is_rejected_before_encoding(self):
        token, _ = make_token()
        prefix, signed = token.split(" ", 1)
        encoded_payload, encoded_signature = signed.split(".", 1)
        encoded_payload = ("A" if encoded_payload[0] != "A" else "B") + encoded_payload[1:]
        broken = f"{prefix} {encoded_payload}.{encoded_signature}"
        conn, response = self.request("/v1/transcode", token=broken, body=b"musicdata")
        self.assertEqual(response.status, 401)
        response.read()
        conn.close()
        self.assertFalse(self.backend.first_chunk.is_set())

    def test_replay_nonce_survives_store_recreation(self):
        token, _ = make_token()
        verifier = TokenVerifier(
            self.secret_file,
            ReplayStore(self.settings.replay_db),
            max_input_bytes=1024,
            max_duration_seconds=120,
        )
        verifier.verify(token, 8)
        verifier_after_restart = TokenVerifier(
            self.secret_file,
            ReplayStore(self.settings.replay_db),
            max_input_bytes=1024,
            max_duration_seconds=120,
        )
        with self.assertRaises(TokenError):
            verifier_after_restart.verify(token, 8)


if __name__ == "__main__":
    unittest.main()
