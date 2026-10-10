#!/usr/bin/env python3
"""Exercise the deployed Navidrome-to-Arasaka worker path without exposing credentials."""

from __future__ import annotations

import argparse
import hashlib
import http.client
import json
import secrets
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path


NAVIDROME_BASE = "http://192.168.137.11:13326"
WORKER_BASE = "http://192.168.137.1:13324"
MUSIC_ROOT = "/srv/onecloud-data/music"
CACHE_ROOT = "/srv/onecloud-data/.cache/navidrome"
WORKER_LOGS = r"C:\ProgramData\ArasakaFFmpegWorker\logs"
WORKER_PYTHON = r"C:\Users\lieyanbang\AppData\Local\Programs\Python\Python314\python.exe"
SERVY_CLI = r"C:\Progra~1\Servy\servy-cli.exe"


class AcceptanceError(RuntimeError):
    pass


class DeployedE2E:
    def __init__(self, args: argparse.Namespace):
        self.args = args
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        self.nonce = time.strftime("%Y%m%d%H%M%S") + secrets.token_hex(3)
        self.main_title = f"CodexWorkerE2E-{self.nonce}-main"
        self.second_title = f"CodexWorkerE2E-{self.nonce}-second"
        self.main_name = f"zz_codex_worker_e2e_{self.nonce}_main.flac"
        self.second_name = f"zz_codex_worker_e2e_{self.nonce}_second.flac"
        self.main_remote = f"{MUSIC_ROOT}/{self.main_name}"
        self.second_remote = f"{MUSIC_ROOT}/{self.second_name}"
        self.local_temp: Path | None = None
        self.created_remote_sources: list[str] = []
        self.recovery_needed = False
        self.params = self._read_auth()

    def _read_auth(self) -> dict[str, str]:
        values: dict[str, str] = {}
        try:
            for line in self.args.credentials.read_text(encoding="utf-8").splitlines():
                name, sep, value = line.partition("=")
                if sep:
                    values[name.strip()] = value.strip()
        except OSError as exc:
            raise AcceptanceError("could not read the protected credential file") from exc
        if not values.get("USERNAME") or not values.get("PASSWORD"):
            raise AcceptanceError("credential file must contain USERNAME and PASSWORD entries")
        salt = secrets.token_urlsafe(18)
        token = hashlib.md5((values["PASSWORD"] + salt).encode("utf-8")).hexdigest()
        return {
            "u": values["USERNAME"],
            "t": token,
            "s": salt,
            "v": "1.16.1",
            "c": "codex-worker-e2e",
            "f": "json",
        }

    def _ssh(self, host: str, key: Path, command: str, port: int | None = None) -> str:
        ssh = self.args.ssh
        cmd = [ssh, "-i", str(key), "-o", "BatchMode=yes", "-o", "ConnectTimeout=10"]
        if port:
            cmd.extend(["-p", str(port)])
        cmd.extend([host, command])
        result = subprocess.run(cmd, capture_output=True, text=True, errors="replace", timeout=45)
        if result.returncode:
            raise AcceptanceError(f"SSH command on {host} failed ({result.returncode})")
        return result.stdout.strip()

    def onecloud(self, command: str) -> str:
        return self._ssh(f"root@{self.args.onecloud_host}", self.args.onecloud_key, command)

    def arasaka(self, command: str) -> str:
        return self._ssh(
            f"{self.args.arasaka_user}@{self.args.arasaka_host}",
            self.args.arasaka_key,
            command,
            self.args.arasaka_ssh_port,
        )

    def upload(self, local_file: Path, remote_file: str) -> None:
        result = subprocess.run(
            [
                self.args.scp,
                "-i",
                str(self.args.onecloud_key),
                str(local_file),
                f"root@{self.args.onecloud_host}:{remote_file}",
            ],
            capture_output=True,
            text=True,
            errors="replace",
            timeout=180,
        )
        if result.returncode:
            raise AcceptanceError("could not upload a temporary lossless fixture to OneCloud")
        self.onecloud(f"chown navidrome:navidrome '{remote_file}' && chmod 0640 '{remote_file}'")
        self.created_remote_sources.append(remote_file)

    def api(self, endpoint: str, extra: dict[str, str] | None = None) -> dict:
        params = dict(self.params)
        if extra:
            params.update(extra)
        url = NAVIDROME_BASE + "/rest/" + endpoint + "?" + urllib.parse.urlencode(params)
        request = urllib.request.Request(url, headers={"User-Agent": "codex-worker-e2e/1"})
        try:
            with self.opener.open(request, timeout=45) as response:
                payload = json.loads(response.read())
        except urllib.error.HTTPError as exc:
            raise AcceptanceError(f"Navidrome API {endpoint} returned HTTP {exc.code}") from None
        except (urllib.error.URLError, TimeoutError, OSError, ValueError) as exc:
            raise AcceptanceError(f"Navidrome API {endpoint} failed ({type(exc).__name__})") from None
        reply = payload.get("subsonic-response", {})
        if reply.get("status") != "ok":
            code = reply.get("error", {}).get("code", "unknown")
            raise AcceptanceError(f"Navidrome API {endpoint} returned Subsonic error {code}")
        return reply

    def worker_health(self) -> bool:
        try:
            result = self.onecloud(
                "curl --noproxy '*' -fsS --max-time 3 "
                f"'{WORKER_BASE}/health' && curl --noproxy '*' -fsS --max-time 3 '{WORKER_BASE}/ready'"
            )
            return '"status":"ok"' in result and '"ready":true' in result
        except AcceptanceError:
            return False

    def wait_worker(self, timeout: int = 90) -> tuple[int, int]:
        deadline = time.monotonic() + timeout
        last_pid = -1
        while time.monotonic() < deadline:
            if self.worker_health():
                last_pid = int(self.worker_pid())
                self.recovery_needed = False
                return last_pid, self.worker_post_count()
            time.sleep(2)
        raise AcceptanceError("Arasaka worker did not become healthy before timeout")

    def worker_pid(self) -> str:
        return self.arasaka(
            "$listener = Get-NetTCPConnection -State Listen -LocalAddress '192.168.137.1' "
            "-LocalPort 13324 -ErrorAction Stop | Select-Object -First 1; "
            "Write-Output $listener.OwningProcess"
        ).splitlines()[-1].strip()

    def worker_post_count(self) -> int:
        command = (
            f"$files = Get-ChildItem -Path '{WORKER_LOGS}\worker.stderr.log*' "
            "-ErrorAction SilentlyContinue; "
            "$postMatches = @($files | Select-String -SimpleMatch 'POST /v1/transcode'); "
            "Write-Output $postMatches.Count"
        )
        return int(self.arasaka(command).splitlines()[-1].strip())

    def worker_part_count(self) -> int:
        result = self.onecloud(
            f"find '{CACHE_ROOT}' -type f -name '*.part' -printf . | wc -c"
        )
        return int(result.splitlines()[-1].strip())

    def worker_cache_count(self) -> int:
        result = self.onecloud(
            f"find '{CACHE_ROOT}' -type f -name '*.mp3' -printf . | wc -c"
        )
        return int(result.splitlines()[-1].strip())

    def worker_cache_bytes(self) -> int:
        result = self.onecloud(f"du -sb '{CACHE_ROOT}' | cut -f1")
        return int(result.splitlines()[-1].strip())

    def kill_worker_process(self, expected_pid: int) -> None:
        command = (
            "$listener = Get-NetTCPConnection -State Listen -LocalAddress '192.168.137.1' "
            "-LocalPort 13324 -ErrorAction Stop | Select-Object -First 1; "
            f"if ($listener.OwningProcess -ne {expected_pid}) {{ throw 'worker PID changed before crash injection' }}; "
            f"$process = Get-CimInstance Win32_Process -Filter 'ProcessId={expected_pid}'; "
            f"if ($process.ExecutablePath -ine '{WORKER_PYTHON}') {{ throw 'unexpected worker process image' }}; "
            f"Stop-Process -Id {expected_pid} -Force; Write-Output 'worker process stopped'"
        )
        self.arasaka(command)
        self.recovery_needed = True

    def stop_worker_service(self) -> None:
        self.arasaka(f"& '{SERVY_CLI}' stop --quiet --name=ArasakaFFmpegWorker")
        self.recovery_needed = True

    def watchdog_restart_count(self) -> int:
        command = (
            f"$files = Get-ChildItem -Path '{WORKER_LOGS}\watchdog.stderr.log*' "
            "-ErrorAction SilentlyContinue; "
            "$restartMatches = @($files | Select-String -SimpleMatch 'restarted Servy service ArasakaFFmpegWorker'); "
            "Write-Output $restartMatches.Count"
        )
        return int(self.arasaka(command).splitlines()[-1].strip())

    def part_count_eventually(self, expected: int, timeout: int = 45) -> int:
        deadline = time.monotonic() + timeout
        last = -1
        while time.monotonic() < deadline:
            last = self.worker_part_count()
            if last == expected:
                return last
            time.sleep(1)
        return last

    def make_fixtures(self) -> tuple[Path, Path]:
        assert self.local_temp is not None
        main = self.local_temp / self.main_name
        second = self.local_temp / self.second_name
        main_wsl = self._wsl_path(main)
        second_wsl = self._wsl_path(second)
        duration = self.args.duration
        self._run(
            [
                self.args.wsl,
                "-e",
                "ffmpeg",
                "-hide_banner",
                "-loglevel",
                "error",
                "-f",
                "lavfi",
                "-i",
                f"anoisesrc=color=white:duration={duration}:sample_rate=44100",
                "-ac",
                "2",
                "-ar",
                "44100",
                "-c:a",
                "flac",
                "-compression_level",
                "0",
                "-metadata",
                f"title={self.main_title}",
                "-metadata",
                "artist=Codex Worker E2E",
                "-y",
                main_wsl,
            ],
            "could not generate the temporary FLAC fixture with WSL FFmpeg",
        )
        second_duration = min(120, duration)
        self._run(
            [
                self.args.wsl,
                "-e",
                "ffmpeg",
                "-hide_banner",
                "-loglevel",
                "error",
                "-f",
                "lavfi",
                "-i",
                f"anoisesrc=color=pink:duration={second_duration}:sample_rate=44100",
                "-ac",
                "2",
                "-ar",
                "44100",
                "-c:a",
                "flac",
                "-compression_level",
                "0",
                "-metadata",
                f"title={self.second_title}",
                "-metadata",
                "artist=Codex Worker E2E",
                "-y",
                second_wsl,
            ],
            "could not generate the second temporary FLAC fixture with WSL FFmpeg",
        )
        return main, second

    def _wsl_path(self, path: Path) -> str:
        result = subprocess.run(
            [self.args.wsl, "-e", "wslpath", "-u", str(path)],
            capture_output=True,
            text=True,
            errors="replace",
            timeout=15,
        )
        if result.returncode:
            raise AcceptanceError("could not map the temporary fixture path into WSL")
        return result.stdout.strip()

    @staticmethod
    def _run(command: list[str], failure: str, timeout: int = 1200) -> str:
        result = subprocess.run(command, capture_output=True, text=True, errors="replace", timeout=timeout)
        if result.returncode:
            raise AcceptanceError(failure)
        return result.stdout.strip()

    def ffprobe(self, path: Path) -> dict:
        result = self._run(
            [
                self.args.wsl,
                "-e",
                "ffprobe",
                "-v",
                "error",
                "-show_entries",
                "stream=codec_name,bit_rate,sample_rate,channels:format=duration,bit_rate",
                "-of",
                "json",
                self._wsl_path(path),
            ],
            "ffprobe could not inspect the returned stream",
            timeout=60,
        )
        return json.loads(result)

    def find_song_id(self, title: str) -> tuple[str, dict]:
        reply = self.api("search3.view", {"query": title, "songCount": "10", "albumCount": "0", "artistCount": "0"})
        songs = reply.get("searchResult3", {}).get("song", [])
        if isinstance(songs, dict):
            songs = [songs]
        for song in songs:
            if song.get("title") == title:
                return str(song["id"]), song
        raise AcceptanceError(f"Navidrome scanner did not index test track {title}")

    def wait_song(self, title: str, timeout: int = 180) -> tuple[str, dict]:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                return self.find_song_id(title)
            except AcceptanceError:
                time.sleep(2)
        raise AcceptanceError(f"timed out waiting for scanner to index {title}")

    def stream_request(self, song_id: str, extra: dict[str, str] | None = None, headers: dict[str, str] | None = None):
        params = {"id": song_id, "format": "mp3", "maxBitRate": "64"}
        if extra:
            params.update(extra)
        query = dict(self.params)
        query.update(params)
        request = urllib.request.Request(
            NAVIDROME_BASE + "/rest/stream.view?" + urllib.parse.urlencode(query),
            headers={"User-Agent": "codex-worker-e2e/1", **(headers or {})},
        )
        try:
            return self.opener.open(request, timeout=self.args.stream_timeout)
        except urllib.error.HTTPError as exc:
            raise AcceptanceError(f"stream request returned HTTP {exc.code}") from None
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            raise AcceptanceError(f"stream request failed ({type(exc).__name__})") from None

    def stream_to_file(self, song_id: str, path: Path, extra: dict[str, str] | None = None) -> dict:
        started = time.monotonic()
        first_byte_ms: int | None = None
        part_count_on_first = -1
        with self.stream_request(song_id, extra) as response, path.open("wb") as output:
            if response.status != 200:
                raise AcceptanceError(f"expected HTTP 200 for full stream, got {response.status}")
            if not response.headers.get("Content-Type", "").lower().startswith("audio/"):
                raise AcceptanceError("stream response did not contain an audio content type")
            block = response.read(64 * 1024)
            if not block:
                raise AcceptanceError("stream returned no audio bytes")
            first_byte_ms = int((time.monotonic() - started) * 1000)
            output.write(block)
            part_count_on_first = self.worker_part_count()
            while True:
                block = response.read(64 * 1024)
                if not block:
                    break
                output.write(block)
        return {
            "status": 200,
            "bytes": path.stat().st_size,
            "first_byte_ms": first_byte_ms,
            "total_ms": int((time.monotonic() - started) * 1000),
            "part_count_on_first": part_count_on_first,
        }

    def stream_short(self, song_id: str, extra: dict[str, str] | None = None, headers: dict[str, str] | None = None, limit: int = 4096) -> tuple[int, bytes, dict]:
        with self.stream_request(song_id, extra, headers) as response:
            data = response.read(limit)
            return response.status, data, dict(response.headers)

    def wait_for_nav_authenticated(self, timeout: int = 90) -> None:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                self.api("ping.view")
                return
            except AcceptanceError:
                time.sleep(2)
        raise AcceptanceError("Navidrome did not become ready after restart")

    def run(self) -> dict:
        report: dict = {"started": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "checks": {}}
        with tempfile.TemporaryDirectory(prefix="navidrome-worker-e2e-") as temp_dir:
            self.local_temp = Path(temp_dir)
            main_file, second_file = self.make_fixtures()
            self.upload(main_file, self.main_remote)
            self.upload(second_file, self.second_remote)
            self.api("ping.view")
            self.api("startScan.view")
            main_id, main_song = self.wait_song(self.main_title)
            second_id, _ = self.wait_song(self.second_title)
            report["main_media_id"] = main_id
            report["second_media_id"] = second_id
            report["main_source_bytes"] = main_file.stat().st_size
            report["second_source_bytes"] = second_file.stat().st_size
            report["checks"]["auth_and_scan"] = "PASS"

            self.wait_worker()
            cache_before_crash = self.worker_cache_count()
            parts_before_crash = self.worker_part_count()
            old_pid = int(self.worker_pid())
            failed_bytes = 0
            first_data = b""
            with self.stream_request(main_id) as interrupted:
                first_data = interrupted.read(64 * 1024)
                if not first_data:
                    raise AcceptanceError("first stream produced no bytes before crash injection")
                part_count_during = self.worker_part_count()
                self.kill_worker_process(old_pid)
                failed_bytes += len(first_data)
                try:
                    while True:
                        block = interrupted.read(64 * 1024)
                        if not block:
                            break
                        failed_bytes += len(block)
                except (http.client.HTTPException, urllib.error.URLError, TimeoutError, OSError) as exc:
                    report["crash_stream_error"] = type(exc).__name__
            expected_bytes = int(float(main_song.get("duration", self.args.duration)) * 192000 / 8)
            if failed_bytes >= expected_bytes * 0.8:
                raise AcceptanceError("worker crash injection did not interrupt the active transcode")
            new_pid, _ = self.wait_worker()
            if new_pid == old_pid:
                raise AcceptanceError("Servy did not replace the crashed worker process")
            parts_after_crash = self.part_count_eventually(0)
            cache_after_crash = self.worker_cache_count()
            if cache_after_crash != cache_before_crash:
                raise AcceptanceError("incomplete worker output was published to the permanent cache")
            report["checks"]["servy_crash_restart"] = "PASS"
            report["checks"]["interrupted_part_not_published"] = "PASS"
            report["crash_injection"] = {
                "pid_before": old_pid,
                "pid_after": new_pid,
                "bytes_received": failed_bytes,
                "expected_full_bytes_approx": expected_bytes,
                "part_count_during": part_count_during,
                "part_count_before": parts_before_crash,
                "part_count_after": parts_after_crash,
            }

            retry_path = self.local_temp / "main.mp3"
            retry = self.stream_to_file(main_id, retry_path)
            probe = self.ffprobe(retry_path)
            stream = probe.get("streams", [{}])[0]
            bitrate = int(stream.get("bit_rate") or probe.get("format", {}).get("bit_rate") or 0)
            if stream.get("codec_name") != "mp3" or bitrate != 192000:
                raise AcceptanceError(f"ffprobe did not verify fixed MP3 192 kbps (codec={stream.get('codec_name')}, bitrate={bitrate})")
            if retry["part_count_on_first"] < 1:
                raise AcceptanceError("cache miss did not expose an active .part file while the client was receiving bytes")
            if retry["first_byte_ms"] >= retry["total_ms"]:
                raise AcceptanceError("client did not receive the cache miss before the stream completed")
            report["checks"]["live_stream_to_disk_cache"] = "PASS"
            report["retry_stream"] = retry
            report["ffprobe"] = {
                "codec_name": stream.get("codec_name"),
                "bit_rate": bitrate,
                "sample_rate": stream.get("sample_rate"),
                "channels": stream.get("channels"),
                "duration": probe.get("format", {}).get("duration"),
            }

            post_after_retry = self.worker_post_count()
            cached_path = self.local_temp / "cache-hit.mp3"
            cache_hit = self.stream_to_file(main_id, cached_path)
            post_after_hit = self.worker_post_count()
            if post_after_hit != post_after_retry or cached_path.stat().st_size != retry_path.stat().st_size:
                raise AcceptanceError("completed cache playback invoked the worker or changed byte length")
            report["checks"]["cache_hit_without_worker"] = "PASS"
            report["cache_hit"] = cache_hit

            range_status, range_data, range_headers = self.stream_short(
                main_id, headers={"Range": "bytes=0-4095"}, limit=4096
            )
            if range_status != 206 or len(range_data) != 4096 or not range_headers.get("Content-Range", "").startswith("bytes 0-4095/"):
                raise AcceptanceError("completed MP3 cache did not serve HTTP Range 206")
            report["checks"]["cached_range_206"] = "PASS"

            cache_before_offset = self.worker_cache_count()
            posts_before_offset = self.worker_post_count()
            offset_path = self.local_temp / "offset.mp3"
            offset_result = self.stream_to_file(main_id, offset_path, {"timeOffset": "60"})
            cache_after_offset = self.worker_cache_count()
            posts_after_offset = self.worker_post_count()
            if cache_after_offset != cache_before_offset or posts_after_offset <= posts_before_offset:
                raise AcceptanceError("offset transcode did not bypass the full-song cache")
            report["checks"]["offset_bypasses_full_cache"] = "PASS"
            report["offset_stream"] = offset_result

            second_path = self.local_temp / "second.mp3"
            second_result = self.stream_to_file(second_id, second_path)
            second_probe = self.ffprobe(second_path)
            second_stream = second_probe.get("streams", [{}])[0]
            if second_stream.get("codec_name") != "mp3" or int(second_stream.get("bit_rate") or 0) != 192000:
                raise AcceptanceError("second independent media request did not produce MP3 192 kbps")
            report["checks"]["multiple_media_requests"] = "PASS"
            report["second_stream"] = second_result

            count_before_restart = self.worker_post_count()
            self.onecloud("systemctl restart navidrome")
            self.wait_for_nav_authenticated()
            restart_cache_path = self.local_temp / "after-restart.mp3"
            restart_cache = self.stream_to_file(main_id, restart_cache_path)
            if self.worker_post_count() != count_before_restart:
                raise AcceptanceError("Navidrome restart lost the completed cache and re-invoked the worker")
            report["checks"]["cache_persists_across_navidrome_restart"] = "PASS"
            report["after_restart"] = restart_cache

            watchdog_before = self.watchdog_restart_count()
            self.stop_worker_service()
            deadline = time.monotonic() + 15
            while time.monotonic() < deadline and self.worker_health():
                time.sleep(1)
            if self.worker_health():
                raise AcceptanceError("Servy stop did not make the worker unavailable")
            offline_cache_path = self.local_temp / "worker-offline-cache.mp3"
            offline_cache = self.stream_to_file(main_id, offline_cache_path)
            raw_status, raw_data, _ = self.stream_short(main_id, {"format": "raw"}, limit=4)
            if raw_status != 200 or raw_data != b"fLaC":
                raise AcceptanceError("raw lossless playback failed while the worker service was stopped")
            report["checks"]["cached_and_raw_playback_while_worker_stopped"] = "PASS"
            report["offline_cache"] = offline_cache

            deadline = time.monotonic() + 90
            watchdog_after = watchdog_before
            while time.monotonic() < deadline:
                watchdog_after = self.watchdog_restart_count()
                if watchdog_after > watchdog_before and self.worker_health():
                    break
                time.sleep(2)
            if watchdog_after <= watchdog_before or not self.worker_health():
                raise AcceptanceError("HTTP health watchdog did not restart the stopped worker")
            self.recovery_needed = False
            report["checks"]["http_watchdog_recovery"] = "PASS"
            report["watchdog_restarts"] = watchdog_after - watchdog_before
            report["cache_mp3_count"] = self.worker_cache_count()
            report["cache_bytes"] = self.worker_cache_bytes()
            report["worker_post_count"] = self.worker_post_count()
            report["checks"]["all"] = "PASS"

            if not self.args.keep_source:
                self.cleanup_sources()
                self.api("startScan.view")
                self.wait_song_removed(self.main_title)
                self.wait_song_removed(self.second_title)
                report["checks"]["fixture_cleanup_and_rescan"] = "PASS"
                report["cache_mp3_count_after_fixture_cleanup"] = self.worker_cache_count()
                report["cache_bytes_after_fixture_cleanup"] = self.worker_cache_bytes()
        return report

    def wait_song_removed(self, title: str, timeout: int = 90) -> None:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            reply = self.api("search3.view", {"query": title, "songCount": "10", "albumCount": "0", "artistCount": "0"})
            songs = reply.get("searchResult3", {}).get("song", [])
            if isinstance(songs, dict):
                songs = [songs]
            if not any(song.get("title") == title for song in songs):
                return
            time.sleep(2)
        raise AcceptanceError(f"scanner still reports removed test track {title}")

    def cleanup_sources(self) -> None:
        if not self.created_remote_sources:
            return
        quoted = " ".join("'" + path.replace("'", "'\\''") + "'" for path in self.created_remote_sources)
        self.onecloud(f"rm -f -- {quoted} && sync")
        self.created_remote_sources.clear()


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--credentials", type=Path, required=True, help="protected USERNAME/PASSWORD file outside the repository")
    parser.add_argument("--onecloud-key", type=Path, required=True)
    parser.add_argument("--arasaka-key", type=Path, required=True)
    parser.add_argument("--onecloud-host", default="192.168.137.11")
    parser.add_argument("--arasaka-host", default="arasaka.tailc57112.ts.net")
    parser.add_argument("--arasaka-user", default="lieyanbang")
    parser.add_argument("--arasaka-ssh-port", type=int, default=13988)
    parser.add_argument("--ssh", default="ssh.exe")
    parser.add_argument("--scp", default="scp.exe")
    parser.add_argument("--wsl", default="wsl.exe")
    parser.add_argument("--duration", type=int, default=900, help="long noise fixture duration in seconds")
    parser.add_argument("--stream-timeout", type=int, default=180)
    parser.add_argument("--keep-source", action="store_true", help="leave the uniquely named temporary tracks in the library")
    args = parser.parse_args()
    for label, path in (("credentials", args.credentials), ("OneCloud key", args.onecloud_key), ("Arasaka key", args.arasaka_key)):
        if not path.is_file():
            parser.error(f"{label} file does not exist")
    if args.duration < 900:
        parser.error("duration must be at least 900 seconds for reliable live crash recovery validation")
    return args


def main() -> int:
    runner: DeployedE2E | None = None
    try:
        runner = DeployedE2E(parse_args())
        report = runner.run()
    except AcceptanceError as exc:
        print(f"FAIL: {exc}")
        return 1
    except (subprocess.SubprocessError, OSError) as exc:
        print(f"FAIL: {type(exc).__name__}")
        return 1
    finally:
        if runner is not None and runner.recovery_needed:
            try:
                if not runner.worker_health():
                    runner.arasaka(f"& '{SERVY_CLI}' restart --quiet --name=ArasakaFFmpegWorker")
                    runner.wait_worker()
                print("RECOVERY: Arasaka worker is healthy")
            except Exception:
                print("RECOVERY: manual attention required for ArasakaFFmpegWorker")
    print(json.dumps(report, indent=2, sort_keys=True))
    return 0 if report.get("checks", {}).get("all") == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
