from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path


def _int_env(name: str, default: int) -> int:
    raw = os.environ.get(name)
    if raw is None:
        return default
    value = int(raw)
    if value <= 0:
        raise ValueError(f"{name} must be positive")
    return value


@dataclass(frozen=True)
class Settings:
    bind: str = "192.168.137.1"
    port: int = 13324
    ffmpeg: str = r"C:\ProgramData\ArasakaFFmpegWorker\ffmpeg\bin\ffmpeg.exe"
    secret_file: Path = Path(r"C:\ProgramData\ArasakaFFmpegWorker\secret.key")
    replay_db: Path = Path(r"C:\ProgramData\ArasakaFFmpegWorker\state\replay.sqlite3")
    max_input_bytes: int = 2 * 1024**3
    max_duration_seconds: int = 6 * 60 * 60

    @classmethod
    def from_environment(cls) -> Settings:
        return cls(
            bind=os.environ.get("ARASAKA_WORKER_BIND", "192.168.137.1"),
            port=_int_env("ARASAKA_WORKER_PORT", 13324),
            ffmpeg=os.environ.get(
                "ARASAKA_WORKER_FFMPEG",
                r"C:\ProgramData\ArasakaFFmpegWorker\ffmpeg\bin\ffmpeg.exe",
            ),
            secret_file=Path(
                os.environ.get(
                    "ARASAKA_WORKER_SECRET_FILE",
                    r"C:\ProgramData\ArasakaFFmpegWorker\secret.key",
                )
            ),
            replay_db=Path(
                os.environ.get(
                    "ARASAKA_WORKER_REPLAY_DB",
                    r"C:\ProgramData\ArasakaFFmpegWorker\state\replay.sqlite3",
                )
            ),
            max_input_bytes=_int_env("ARASAKA_WORKER_MAX_INPUT_BYTES", 2 * 1024**3),
            max_duration_seconds=_int_env("ARASAKA_WORKER_MAX_DURATION_SECONDS", 6 * 60 * 60),
        )
