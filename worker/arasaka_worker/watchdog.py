from __future__ import annotations

import logging
import os
import subprocess
import time
import urllib.error
import urllib.request


logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")
log = logging.getLogger("arasaka_worker.watchdog")


def _positive_env(name: str, default: int) -> int:
    value = int(os.environ.get(name, default))
    if value <= 0:
        raise ValueError(f"{name} must be positive")
    return value


def _healthy(base_url: str) -> bool:
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    for path in ("/health", "/ready"):
        try:
            with opener.open(base_url + path, timeout=3) as response:
                if response.status != 200:
                    return False
                response.read(1024)
        except (OSError, urllib.error.URLError):
            return False
    return True


def main() -> None:
    base_url = os.environ.get("ARASAKA_WATCHDOG_URL", "http://192.168.137.1:13324")
    interval = _positive_env("ARASAKA_WATCHDOG_INTERVAL_SECONDS", 10)
    max_failures = _positive_env("ARASAKA_WATCHDOG_MAX_FAILURES", 3)
    cooldown = _positive_env("ARASAKA_WATCHDOG_RESTART_COOLDOWN_SECONDS", 120)
    cli = os.environ.get("ARASAKA_SERVY_CLI", r"C:\Progra~1\Servy\servy-cli.exe")
    service_name = os.environ.get("ARASAKA_WORKER_SERVICE_NAME", "ArasakaFFmpegWorker")
    failures = 0
    next_restart = 0.0
    startup_grace = time.monotonic() + 20
    log.info("watchdog probing %s for service %s", base_url, service_name)
    while True:
        if _healthy(base_url):
            if failures:
                log.info("worker health and readiness recovered")
            failures = 0
        else:
            failures += 1
            log.warning("worker health/readiness probe failed (%d/%d)", failures, max_failures)
            now = time.monotonic()
            if failures >= max_failures and now >= startup_grace and now >= next_restart:
                try:
                    result = subprocess.run(
                        [cli, "restart", "--quiet", f"--name={service_name}"],
                        stdin=subprocess.DEVNULL,
                        stdout=subprocess.PIPE,
                        stderr=subprocess.STDOUT,
                        timeout=30,
                        check=False,
                        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
                    )
                    if result.returncode == 0:
                        log.warning("restarted Servy service %s after failed HTTP checks", service_name)
                    else:
                        log.error("Servy restart command failed for %s (exit=%d)", service_name, result.returncode)
                except (OSError, subprocess.SubprocessError) as exc:
                    log.error("could not request Servy restart for %s: %s", service_name, type(exc).__name__)
                failures = 0
                next_restart = time.monotonic() + cooldown
        time.sleep(interval)


if __name__ == "__main__":
    main()
