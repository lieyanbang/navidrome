from __future__ import annotations

import hashlib
import logging
import subprocess
import threading
import time
from collections import deque
from collections.abc import Callable

from .config import Settings
from .tokens import Claims


log = logging.getLogger("arasaka_worker.backend")


class TranscodeError(RuntimeError):
    pass


class FFmpegBackend:
    def __init__(self, settings: Settings):
        self.settings = settings
        self._probe_lock = threading.Lock()
        self._probe_until = 0.0
        self._probe_ready = False

    def ready(self) -> bool:
        now = time.monotonic()
        with self._probe_lock:
            if now < self._probe_until:
                return self._probe_ready
            try:
                result = subprocess.run(
                    [self.settings.ffmpeg, "-hide_banner", "-encoders"],
                    stdin=subprocess.DEVNULL,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.STDOUT,
                    timeout=5,
                    check=False,
                    creationflags=_creation_flags(),
                )
                self._probe_ready = result.returncode == 0 and b"libmp3lame" in result.stdout
            except (OSError, subprocess.SubprocessError):
                self._probe_ready = False
            self._probe_until = now + (30 if self._probe_ready else 5)
            return self._probe_ready

    def run(
        self,
        claims: Claims,
        source,
        content_length: int,
        write_chunk: Callable[[bytes], None],
    ) -> tuple[str, int]:
        if not self.ready():
            raise TranscodeError("FFmpeg is unavailable or lacks libmp3lame")
        argv = [self.settings.ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error"]
        if claims.offset_seconds:
            argv.extend(["-i", "pipe:0", "-ss", str(claims.offset_seconds)])
        else:
            argv.extend(["-i", "pipe:0"])
        argv.extend(
            [
                "-map",
                "0:a:0",
                "-map_metadata",
                "0",
                "-vn",
                "-sn",
                "-dn",
                "-c:a",
                "libmp3lame",
                "-threads:a",
                "2",
                "-b:a",
                "192k",
                "-abr",
                "0",
                "-f",
                "mp3",
                "pipe:1",
            ]
        )
        try:
            process = subprocess.Popen(
                argv,
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                bufsize=0,
                creationflags=_creation_flags(),
            )
        except OSError as exc:
            raise TranscodeError("could not start FFmpeg") from exc

        input_state: dict[str, object] = {"bytes": 0, "error": None}
        stop_input = threading.Event()
        stderr_tail: deque[bytes] = deque()
        stderr_size = 0
        stderr_lock = threading.Lock()

        def feed_input() -> None:
            try:
                remaining = content_length
                while remaining:
                    if stop_input.is_set():
                        raise TranscodeError("transcode stopped before source upload completed")
                    block = source.read(min(64 * 1024, remaining))
                    if not block:
                        raise TranscodeError("source upload was truncated")
                    view = memoryview(block)
                    while view:
                        written = process.stdin.write(view)
                        if not written:
                            raise BrokenPipeError("FFmpeg closed its input pipe")
                        view = view[written:]
                    input_state["bytes"] = int(input_state["bytes"]) + len(block)
                    remaining -= len(block)
                process.stdin.close()
            except (BrokenPipeError, OSError, TranscodeError) as exc:
                input_state["error"] = exc
                stop_input.set()
                _kill(process)
            finally:
                if process.stdin and not process.stdin.closed:
                    try:
                        process.stdin.close()
                    except OSError:
                        pass

        def drain_stderr() -> None:
            nonlocal stderr_size
            while True:
                block = process.stderr.read(4096)
                if not block:
                    return
                with stderr_lock:
                    stderr_tail.append(block)
                    stderr_size += len(block)
                    while stderr_size > 16 * 1024 and stderr_tail:
                        removed = stderr_tail.popleft()
                        stderr_size -= len(removed)

        input_thread = threading.Thread(target=feed_input, name="worker-source", daemon=True)
        stderr_thread = threading.Thread(target=drain_stderr, name="worker-stderr", daemon=True)
        input_thread.start()
        stderr_thread.start()
        duration_seconds = claims.duration_ms / 1000
        timeout_seconds = min(max(duration_seconds * 3 + 120, 120), 12 * 60 * 60)
        timed_out = threading.Event()

        def timeout_process() -> None:
            if process.poll() is None:
                timed_out.set()
                _kill(process)

        timer = threading.Timer(timeout_seconds, timeout_process)
        timer.daemon = True
        timer.start()
        digest = hashlib.sha256()
        output_bytes = 0
        output_limit = (claims.duration_ms * 192_000 * 11 // 80_000) + 1024 * 1024
        try:
            while True:
                block = process.stdout.read(64 * 1024)
                if not block:
                    break
                if output_bytes + len(block) > output_limit:
                    raise TranscodeError("FFmpeg output exceeded the signed duration limit")
                write_chunk(block)
                digest.update(block)
                output_bytes += len(block)
            input_thread.join(timeout=10)
            if input_thread.is_alive():
                raise TranscodeError("source upload did not finish")
            return_code = process.wait(timeout=10)
            if timed_out.is_set():
                raise TranscodeError("transcode exceeded its execution limit")
            if input_state["error"] is not None:
                raise TranscodeError("source upload or encoder input failed") from input_state["error"]
            if int(input_state["bytes"]) != content_length:
                raise TranscodeError("encoder did not receive the complete source")
            if return_code != 0:
                with stderr_lock:
                    diagnostic = b"".join(stderr_tail).decode("utf-8", "replace")[-2048:]
                diagnostic = " ".join(diagnostic.split())
                log.error("FFmpeg failed for media_id=%s: %s", claims.media_id, diagnostic)
                raise TranscodeError("FFmpeg exited unsuccessfully")
            if output_bytes <= 0:
                raise TranscodeError("FFmpeg produced no output")
            return digest.hexdigest(), output_bytes
        except Exception:
            stop_input.set()
            _kill(process)
            raise
        finally:
            timer.cancel()
            if process.poll() is None:
                _kill(process)
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                _kill(process)
            input_thread.join(timeout=2)
            stderr_thread.join(timeout=2)
            if process.stdout:
                process.stdout.close()
            if process.stderr:
                process.stderr.close()


def _creation_flags() -> int:
    return getattr(subprocess, "CREATE_NO_WINDOW", 0)


def _kill(process: subprocess.Popen) -> None:
    if process.poll() is None:
        try:
            process.kill()
        except OSError:
            pass
