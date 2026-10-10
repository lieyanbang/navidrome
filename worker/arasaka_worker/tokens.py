from __future__ import annotations

import base64
import hashlib
import hmac
import json
import re
import sqlite3
import time
from dataclasses import dataclass
from pathlib import Path


TOKEN_PREFIX = "Bearer "
PRESET = "mp3-cbr-192-v1"
_NONCE_RE = re.compile(r"^[A-Za-z0-9_-]{20,64}$")
_TOKEN_FIELDS = {
    "v",
    "media_id",
    "source_size",
    "source_mtime_ns",
    "duration_ms",
    "offset_seconds",
    "preset",
    "exp",
    "nonce",
}


class TokenError(ValueError):
    pass


@dataclass(frozen=True)
class Claims:
    media_id: str
    source_size: int
    source_mtime_ns: int
    duration_ms: int
    offset_seconds: int
    expires_at: int
    nonce: str


class ReplayStore:
    """Durable one-use token nonces, so a worker restart cannot reopen a token."""

    def __init__(self, path: Path):
        path.parent.mkdir(parents=True, exist_ok=True)
        self.path = path
        with self._connect() as db:
            db.execute(
                "CREATE TABLE IF NOT EXISTS used_nonces ("
                "nonce TEXT PRIMARY KEY, expires_at INTEGER NOT NULL)"
            )

    def _connect(self) -> sqlite3.Connection:
        db = sqlite3.connect(self.path, timeout=5)
        db.execute("PRAGMA busy_timeout=5000")
        return db

    def consume(self, nonce: str, expires_at: int, now: int) -> None:
        try:
            with self._connect() as db:
                db.execute("BEGIN IMMEDIATE")
                db.execute("DELETE FROM used_nonces WHERE expires_at < ?", (now - 60,))
                db.execute(
                    "INSERT INTO used_nonces(nonce, expires_at) VALUES (?, ?)",
                    (nonce, expires_at),
                )
                db.commit()
        except sqlite3.IntegrityError as exc:
            raise TokenError("token has already been used") from exc


class TokenVerifier:
    def __init__(
        self,
        key_file: Path,
        replay_store: ReplayStore,
        *,
        max_input_bytes: int,
        max_duration_seconds: int,
        max_lifetime_seconds: int = 120,
    ):
        encoded = key_file.read_text(encoding="ascii").strip()
        self.key = bytes.fromhex(encoded)
        if len(self.key) < 32:
            raise ValueError("worker token key must contain at least 32 bytes")
        self.replay_store = replay_store
        self.max_input_bytes = max_input_bytes
        self.max_duration_ms = max_duration_seconds * 1000
        self.max_lifetime_seconds = max_lifetime_seconds

    def verify(self, authorization: str | None, content_length: int) -> Claims:
        if not authorization or not authorization.startswith(TOKEN_PREFIX):
            raise TokenError("missing bearer token")
        if len(authorization) > 4096:
            raise TokenError("worker token is too large")
        token = authorization[len(TOKEN_PREFIX) :]
        parts = token.split(".")
        if len(parts) != 2:
            raise TokenError("malformed bearer token")
        encoded_payload, encoded_signature = parts
        try:
            received = _b64url_decode(encoded_signature)
            payload = _b64url_decode(encoded_payload)
            signed_bytes = encoded_payload.encode("ascii")
        except (ValueError, UnicodeError) as exc:
            raise TokenError("malformed bearer token") from exc
        expected = hmac.new(self.key, signed_bytes, hashlib.sha256).digest()
        if not hmac.compare_digest(expected, received):
            raise TokenError("invalid bearer token signature")
        try:
            value = json.loads(payload)
        except (json.JSONDecodeError, UnicodeDecodeError) as exc:
            raise TokenError("invalid bearer token payload") from exc
        if not isinstance(value, dict) or set(value) != _TOKEN_FIELDS:
            raise TokenError("unexpected bearer token fields")
        if type(value["v"]) is not int or value["v"] != 1 or value["preset"] != PRESET:
            raise TokenError("unsupported worker token version or preset")
        media_id = value["media_id"]
        if not isinstance(media_id, str) or not media_id or len(media_id) > 256 or any(ord(c) < 32 for c in media_id):
            raise TokenError("invalid media identity")
        size = _positive_int(value["source_size"], "source size")
        mtime_ns = _nonnegative_int(value["source_mtime_ns"], "source modification time")
        duration_ms = _positive_int(value["duration_ms"], "duration")
        offset = _nonnegative_int(value["offset_seconds"], "offset")
        expires_at = _positive_int(value["exp"], "expiry")
        nonce = value["nonce"]
        if not isinstance(nonce, str) or not _NONCE_RE.fullmatch(nonce):
            raise TokenError("invalid token nonce")
        now = int(time.time())
        if expires_at < now or expires_at > now + self.max_lifetime_seconds:
            raise TokenError("expired or overlong worker token")
        if size > self.max_input_bytes or size != content_length:
            raise TokenError("source length is outside the authorized limit")
        if duration_ms > self.max_duration_ms or offset >= (duration_ms + 999) // 1000:
            raise TokenError("duration or offset is outside the authorized limit")
        self.replay_store.consume(nonce, expires_at, now)
        return Claims(media_id, size, mtime_ns, duration_ms, offset, expires_at, nonce)


def _b64url_decode(value: str) -> bytes:
    if not value or not re.fullmatch(r"[A-Za-z0-9_-]+", value):
        raise ValueError("invalid base64url value")
    return base64.urlsafe_b64decode(value + "=" * (-len(value) % 4))


def _positive_int(value: object, label: str) -> int:
    if type(value) is not int or value <= 0:
        raise TokenError(f"invalid {label}")
    return value


def _nonnegative_int(value: object, label: str) -> int:
    if type(value) is not int or value < 0:
        raise TokenError(f"invalid {label}")
    return value
