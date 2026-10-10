from __future__ import annotations

import json
import logging
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from .backend import FFmpegBackend
from .config import Settings
from .tokens import ReplayStore, TokenError, TokenVerifier


logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(name)s %(message)s",
)
log = logging.getLogger("arasaka_worker")


class WorkerService:
    def __init__(self, settings: Settings, backend=None, verifier=None):
        self.settings = settings
        self.backend = backend or FFmpegBackend(settings)
        if verifier is None:
            replay = ReplayStore(settings.replay_db)
            verifier = TokenVerifier(
                settings.secret_file,
                replay,
                max_input_bytes=settings.max_input_bytes,
                max_duration_seconds=settings.max_duration_seconds,
            )
        self.verifier = verifier
        self._slot = threading.BoundedSemaphore(1)

    def handler(self):
        service = self

        class RequestHandler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"
            server_version = "ArasakaFFmpegWorker/1"
            sys_version = ""

            def log_message(self, fmt: str, *args) -> None:
                log.info("client=%s %s", self.client_address[0], fmt % args)

            def do_GET(self) -> None:
                if self.path == "/health":
                    self._json(200, {"status": "ok"})
                    return
                if self.path == "/ready":
                    ready = service.backend.ready()
                    self._json(200 if ready else 503, {"ready": ready})
                    return
                self._json(404, {"error": "not found"})

            def do_POST(self) -> None:
                if self.path != "/v1/transcode":
                    self._json(404, {"error": "not found"})
                    return
                if self.headers.get_all("Transfer-Encoding"):
                    self._json(400, {"error": "transfer encoding is not accepted"})
                    return
                if len(self.headers.get_all("Authorization") or []) != 1:
                    self._json(401, {"error": "single bearer token required"})
                    return
                content_types = self.headers.get_all("Content-Type") or []
                if len(content_types) != 1 or content_types[0].lower() != "application/octet-stream":
                    self._json(415, {"error": "application/octet-stream required"})
                    return
                raw_length = self.headers.get("Content-Length")
                content_lengths = self.headers.get_all("Content-Length") or []
                if len(content_lengths) != 1 or raw_length is None or not raw_length.isdecimal():
                    self._json(411, {"error": "content length required"})
                    return
                content_length = int(raw_length)
                if content_length <= 0 or content_length > service.settings.max_input_bytes:
                    self._json(413, {"error": "source length outside limit"})
                    return
                try:
                    claims = service.verifier.verify(self.headers.get("Authorization"), content_length)
                except TokenError as exc:
                    self._json(401, {"error": str(exc)})
                    return
                if not service._slot.acquire(blocking=False):
                    self._json(503, {"error": "worker busy"})
                    return
                try:
                    if not service.backend.ready():
                        self._json(503, {"error": "worker not ready"})
                        return
                    self._stream_transcode(claims, content_length)
                finally:
                    service._slot.release()

            def _json(self, status: int, payload: dict[str, object]) -> None:
                body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.send_header("Connection", "close")
                self.end_headers()
                self.wfile.write(body)
                self.close_connection = True

            def _stream_transcode(self, claims, content_length: int) -> None:
                self.send_response(200)
                self.send_header("Content-Type", "audio/mpeg")
                self.send_header("Transfer-Encoding", "chunked")
                self.send_header(
                    "Trailer",
                    "X-Transcode-Status, X-Output-SHA256, X-Output-Bytes",
                )
                self.send_header("Cache-Control", "no-store")
                self.end_headers()

                def write_chunk(block: bytes) -> None:
                    self.wfile.write(f"{len(block):X}\r\n".encode("ascii"))
                    self.wfile.write(block)
                    self.wfile.write(b"\r\n")
                    self.wfile.flush()

                status = "failed"
                digest = ""
                output_bytes = 0
                try:
                    digest, output_bytes = service.backend.run(
                        claims, _ContentLengthReader(self.rfile, content_length), content_length, write_chunk
                    )
                    status = "complete"
                except (BrokenPipeError, ConnectionResetError, OSError) as exc:
                    self.close_connection = True
                    log.warning("transcode connection ended media_id=%s error=%s", claims.media_id, type(exc).__name__)
                    return
                except Exception as exc:
                    self.close_connection = True
                    log.error("transcode failed media_id=%s error=%s", claims.media_id, type(exc).__name__)
                try:
                    self.wfile.write(b"0\r\n")
                    self.wfile.write(f"X-Transcode-Status: {status}\r\n".encode("ascii"))
                    self.wfile.write(f"X-Output-SHA256: {digest}\r\n".encode("ascii"))
                    self.wfile.write(f"X-Output-Bytes: {output_bytes}\r\n\r\n".encode("ascii"))
                    self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError, OSError):
                    self.close_connection = True

        return RequestHandler

    def serve_forever(self) -> None:
        Path(self.settings.replay_db).parent.mkdir(parents=True, exist_ok=True)
        server = ThreadingHTTPServer((self.settings.bind, self.settings.port), self.handler())
        server.daemon_threads = True
        server.request_queue_size = 8
        log.info("worker listening on %s:%d", self.settings.bind, self.settings.port)
        try:
            server.serve_forever(poll_interval=0.5)
        finally:
            server.server_close()


class _ContentLengthReader:
    def __init__(self, reader, remaining: int):
        self.reader = reader
        self.remaining = remaining

    def read(self, size: int = -1) -> bytes:
        if self.remaining <= 0:
            return b""
        if size < 0 or size > self.remaining:
            size = self.remaining
        block = self.reader.read(size)
        self.remaining -= len(block)
        return block


def main() -> None:
    WorkerService(Settings.from_environment()).serve_forever()


if __name__ == "__main__":
    main()
